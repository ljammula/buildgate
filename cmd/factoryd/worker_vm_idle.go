package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"slices"
	"strings"
	"sync/atomic"
	"time"

	"go.temporal.io/api/workflowservice/v1"

	"buildgate/internal/hostcontrol"
	"buildgate/internal/request"
	"buildgate/internal/sanitize"
	"buildgate/internal/workflow"
)

// With vm_idle_stop_after set, a worker stops buildgate's Docker VM (and the
// Temporal and OpenShell stacks in it) once nothing has needed it for that
// long, and brings it back when a wake is waiting or a worker starts. The
// stop sequence and the machine-wide marker are internal/hostcontrol's
// (StopVMWhenIdle); this file is the decision, which reads request records
// and so cannot live there.
//
// Idle means every one of these, continuously for the whole duration:
//
//	autostart on, and docker_context pins buildgate's own VM (colima-buildgate)
//	every profile's config loads
//	every open Temporal workflow is a RequestWorkflow of a known data dir
//	  whose request is finished, halted or quarantined (no step, no timer)
//	no data dir has a wake-pending marker
//	no worker reports an active request and the gateway lists no sandbox
//	the VM runs no container but buildgate's own stack
//
// Anything it cannot read blocks the stop.

// vmIdleOpenQuery lists the workflows that are still running.
const vmIdleOpenQuery = `ExecutionStatus = "Running"`

// vmIdleWorkflowLister is the slice of client.Client the idle check reads.
type vmIdleWorkflowLister interface {
	ListWorkflow(ctx context.Context, request *workflowservice.ListWorkflowExecutionsRequest) (*workflowservice.ListWorkflowExecutionsResponse, error)
}

// vmIdleWatcher evaluates the idle rule on the worker's one-minute tick.
type vmIdleWatcher struct {
	dp         *deps
	lister     vmIdleWorkflowLister
	after      time.Duration // vm_idle_stop_after; 0 never stops, but still wakes
	meterImage string
	ownDir     string
	// temporalAddress is the Temporal server the worker uses: a marker that
	// says stopped while it answers is stale (a command or doctor -fix
	// started the VM), and is cleared.
	temporalAddress string
	out             io.Writer
	now             func() time.Time
	profiles        func() ([]profileInfo, error)

	idleSince time.Time
	// quiet is set while the VM is stopped and nothing waits on it: the
	// worker's other periodic scans stay off until a wake clears it.
	quiet atomic.Bool
}

func newVMIdleWatcher(dp *deps, after time.Duration, meterImage, ownDir, temporalAddress string) *vmIdleWatcher {
	return &vmIdleWatcher{dp: dp, after: after, meterImage: meterImage, ownDir: ownDir, temporalAddress: temporalAddress, out: os.Stdout, now: time.Now, profiles: loadProfiles}
}

// dataDirs is every profile's data dir plus the worker's own, with the queue
// id of each that has one. why is non-empty when a profile does not load: its
// data dir is unknown, so nothing can be judged idle.
func (w *vmIdleWatcher) dataDirs() (dirs []string, queues map[string]string, why string) {
	profiles, err := w.profiles()
	if err != nil {
		return nil, nil, "profiles cannot be listed: " + sanitize.Line(err.Error())
	}
	for _, p := range profiles {
		if p.LoadErr != nil {
			return nil, nil, fmt.Sprintf("profile %s does not load", p.Name)
		}
	}
	dirs, _ = distinctDataDirs(profiles)
	if w.ownDir != "" && !slices.Contains(dirs, w.ownDir) {
		dirs = append(dirs, w.ownDir)
	}
	queues = map[string]string{}
	for _, dir := range dirs {
		id, found, err := readRequestQueueID(dir)
		if err != nil {
			return nil, nil, "queue id of " + dir + " unreadable"
		}
		if found {
			queues[id] = dir
		}
	}
	return dirs, queues, ""
}

// blocker names what keeps the machine from being idle, or "" when it is.
func (w *vmIdleWatcher) blocker(ctx context.Context) (why string, dirs []string) {
	if !hostcontrol.AutostartEnabled() {
		return hostcontrol.AutostartEnvVar + "=0", nil
	}
	if p, ok := hostcontrol.PinnedColimaProfile(w.dp); !ok || p != hostcontrol.DockerVMProfile {
		return "docker_context does not pin buildgate's VM", nil
	}
	dirs, queues, why := w.dataDirs()
	if why != "" {
		return why, nil
	}
	if why := w.openWorkflowBlocker(ctx, queues); why != "" {
		return why, dirs
	}
	if why := wakePendingBlocker(dirs); why != "" {
		return why, dirs
	}
	active, err := hostcontrol.ActiveBuilds(w.dp, ctx, dirs, w.now())
	if err != nil {
		return "cannot tell whether a build runs: " + sanitize.Line(err.Error()), dirs
	}
	if len(active) > 0 {
		return "in use by " + strings.Join(active, ", "), dirs
	}
	return w.containerBlocker(ctx), dirs
}

// containerBlocker is a container in the VM that is not part of buildgate's
// own stack.
func (w *vmIdleWatcher) containerBlocker(ctx context.Context) string {
	names, err := w.dp.docker.runningContainers(ctx, w.dp.docker.dockerBinary(), "")
	if err != nil {
		return "cannot list the VM's containers: " + sanitize.Line(err.Error())
	}
	for _, name := range names {
		if !slices.Contains(buildgateStackContainers, name) {
			return "container " + sanitize.Line(name) + " runs in the VM"
		}
	}
	return ""
}

func wakePendingBlocker(dirs []string) string {
	for _, dir := range dirs {
		marks, err := request.ListWakePending(dir)
		if err != nil {
			return "wake markers of " + dir + " unreadable"
		}
		if len(marks) > 0 {
			return "a wake is pending for request " + sanitize.Line(marks[0].ID)
		}
	}
	return ""
}

// openWorkflowBlocker lists every running workflow on the Temporal server and
// names the first that is not a settled request of a known data dir.
func (w *vmIdleWatcher) openWorkflowBlocker(ctx context.Context, queues map[string]string) string {
	if w.lister == nil {
		return "Temporal is not connected"
	}
	var token []byte
	for {
		resp, err := w.lister.ListWorkflow(ctx, &workflowservice.ListWorkflowExecutionsRequest{Query: vmIdleOpenQuery, NextPageToken: token})
		if err != nil {
			return "cannot list Temporal's workflows: " + sanitize.Line(err.Error())
		}
		for _, info := range resp.GetExecutions() {
			if why := settledRequestBlocker(info.GetExecution().GetWorkflowId(), info.GetType().GetName(), queues); why != "" {
				return why
			}
		}
		if token = resp.GetNextPageToken(); len(token) == 0 {
			return ""
		}
	}
}

// settledRequestBlocker is "" when workflowID is the RequestWorkflow of a
// request of a known data dir that is finished, halted or quarantined: it
// holds no timer and starts no step until a decision wakes it.
func settledRequestBlocker(workflowID, workflowType string, queues map[string]string) string {
	if workflowType != workflow.RequestWorkflowName {
		return "workflow " + sanitize.Line(workflowID) + " (" + sanitize.Line(workflowType) + ") is open"
	}
	for queueID, dir := range queues {
		requestID, ok := strings.CutPrefix(workflowID, workflow.RequestWorkflowID(queueID, ""))
		if !ok || requestID == "" {
			continue
		}
		r, err := request.Load(dir, requestID)
		switch {
		case err != nil:
			return "request " + sanitize.Line(requestID) + " cannot be read"
		case r.State.Finished() || r.State == request.StateHalted || r.State == request.StateQuarantined:
			return ""
		}
		return "request " + sanitize.Line(requestID) + " is " + string(r.State)
	}
	return "workflow " + sanitize.Line(workflowID) + " belongs to no known data dir"
}

// tick is one minute's decision and reports whether the worker's other
// periodic work (retrying wakes, scanning repositories) should run: it
// should not while the VM is stopped and nothing waits on it, nor while
// another worker is stopping it.
func (w *vmIdleWatcher) tick(ctx context.Context) bool {
	m, found, err := hostcontrol.ReadVMIdleMarker(w.now())
	if err != nil {
		log.Printf("factoryd worker: read the VM idle marker: %v", err)
		return true
	}
	if !found {
		w.quiet.Store(false)
		w.maybeStop(ctx)
		return !hostcontrol.VMIdleMarkerPresent()
	}
	w.idleSince = time.Time{}
	switch {
	case m.State == hostcontrol.VMIdleStopping && w.now().Sub(m.Since) < hostcontrol.VMIdleStaleAfter:
		return false
	case m.State == hostcontrol.VMIdleStopped && !w.wakePending() && w.dp.TemporalHealthy(ctx, w.temporalAddress) != nil:
		w.quiet.Store(true)
		return false
	}
	return w.wake(ctx, "a wake is waiting, or the VM was started by another command")
}

// wakePending reports whether any data dir holds a wake-pending marker. A
// directory it cannot read counts as one: waking is the safe answer.
func (w *vmIdleWatcher) wakePending() bool {
	dirs, _, why := w.dataDirs()
	if why != "" {
		dirs = []string{w.ownDir}
	}
	return wakePendingBlocker(dirs) != ""
}

// wake brings the VM and both stacks back and removes the marker, reporting
// whether it did.
func (w *vmIdleWatcher) wake(ctx context.Context, reason string) bool {
	log.Printf("factoryd worker: starting the VM that was stopped while idle (%s)", reason)
	err := hostcontrol.WakeVMAfterIdleStop(w.dp, ctx, w.out, func(ctx context.Context, out io.Writer) {
		ensureSandboxRuntime(w.dp, ctx, out, w.meterImage)
	})
	if err != nil {
		log.Printf("factoryd worker: start the VM: %v", err)
		return false
	}
	w.quiet.Store(false)
	return true
}

// wakeAtStart is a worker start under a marker: the startup path has just
// started the VM and Temporal, so it only finishes the wake.
func (w *vmIdleWatcher) wakeAtStart(ctx context.Context) {
	if _, found, _ := hostcontrol.ReadVMIdleMarker(w.now()); found {
		w.wake(ctx, "the worker starts")
	}
}

// maybeStop stops the VM once the machine has been idle for the whole
// vm_idle_stop_after.
func (w *vmIdleWatcher) maybeStop(ctx context.Context) {
	if w.after <= 0 {
		return
	}
	why, dirs := w.blocker(ctx)
	if why != "" {
		w.idleSince = time.Time{}
		return
	}
	now := w.now()
	if w.idleSince.IsZero() {
		w.idleSince = now
		return
	}
	if now.Sub(w.idleSince) < w.after {
		return
	}
	w.idleSince = time.Time{}
	log.Printf("factoryd worker: idle for %s, stopping the VM", w.after)
	err := hostcontrol.StopVMWhenIdle(w.dp, ctx, w.out, dirs, w.now, func(ctx context.Context) error {
		if why, _ := w.blocker(ctx); why != "" {
			return errors.New(why)
		}
		return nil
	})
	if err != nil {
		log.Printf("factoryd worker: the VM was not stopped: %v", err)
		return
	}
	w.quiet.Store(true)
	log.Printf("factoryd worker: the VM is stopped; a wake or a worker start brings it back")
}

// gate passes a tick of in on only while the VM is not stopped and idle.
func (w *vmIdleWatcher) gate(ctx context.Context, in <-chan time.Time) <-chan time.Time {
	out := make(chan time.Time)
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case t := <-in:
				if w.quiet.Load() {
					continue
				}
				select {
				case out <- t:
				case <-ctx.Done():
					return
				}
			}
		}
	}()
	return out
}

// vmIdleInfoLine is the one line doctor and status print for a VM stopped
// while idle, or "" when there is none.
func vmIdleInfoLine(now time.Time) string {
	if !hostcontrol.AutostartEnabled() {
		return ""
	}
	m, found, err := hostcontrol.ReadVMIdleMarker(now)
	if err != nil || !found || m.State != hostcontrol.VMIdleStopped {
		return ""
	}
	released := ""
	if m.MemoryBytes > 0 {
		released = fmt.Sprintf(" (%.1f GiB released)", float64(m.MemoryBytes)/(1<<30))
	}
	return fmt.Sprintf("VM stopped while idle since %s%s; a wake or a worker start brings it back", m.Since.Local().Format("2006-01-02 15:04"), released)
}
