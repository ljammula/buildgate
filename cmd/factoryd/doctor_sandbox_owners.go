package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"buildgate/internal/hostcontrol"
	"buildgate/internal/run"
	"buildgate/internal/sandbox"
	"buildgate/internal/sanitize"
	"buildgate/internal/sessionconfig"
	"buildgate/internal/workflow"
)

// sandboxOwner is the run a sandbox the gateway lists belongs to. The run is
// found in its own record of the sandboxes it started (sandbox.
// RecordedSandboxes), never in the gateway's labels, which are cut to the
// gateway's length limit.
type sandboxOwner struct {
	name string
	// dataDir and runID are empty when no run in any data dir doctor knows
	// recorded the sandbox.
	dataDir string
	runID   string
	// lost is true when nothing will remove the sandbox on its own: its run
	// is confirmed terminal, or the factoryd process that owned the run is
	// provably gone (runOwnerDead). A run that may still be building is
	// never lost.
	lost bool
}

// sandboxDataDirDepth is how far under sessionconfig.DataRoot doctor looks
// for a data dir: DataRoot/data, DataRoot/<name>/data, the scratch data dirs
// the live scripts keep one level further down, and the deepest of them,
// scripts/bar.sh's DataRoot/bar/run.<id>/<fixture>/data. A data dir deeper
// than this hides a live build's sandbox from doctor.
const sandboxDataDirDepth = 4

// sandboxDataDirs is every data dir doctor looks in for a sandbox's run:
// its own, each profile's, and each directory holding a runs directory at
// most sandboxDataDirDepth levels under sessionconfig.DataRoot, where a
// profile that was since deleted kept its records. A build can leave a
// sandbox in a data dir that no profile names any more, and no worker then
// starts there to reclaim it.
func sandboxDataDirs(own string) []string {
	var candidates []string
	if own != "" {
		candidates = append(candidates, own)
	}
	if profiles, err := loadProfiles(); err == nil {
		dirs, _ := distinctDataDirs(profiles)
		candidates = append(candidates, dirs...)
	}
	candidates = append(candidates, dataDirsUnder(sessionconfig.DataRoot(), sandboxDataDirDepth)...)
	seen := map[string]bool{}
	var dirs []string
	for _, dir := range candidates {
		abs, err := filepath.Abs(dir)
		if err != nil || seen[abs] {
			continue
		}
		seen[abs] = true
		dirs = append(dirs, abs)
	}
	return dirs
}

// dataDirsUnder returns the directories at most depth levels under root
// that hold a runs directory, in name order. It does not look inside one it
// found: a data dir's workspaces are whole repositories.
func dataDirsUnder(root string, depth int) []string {
	if depth == 0 {
		return nil
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil
	}
	var dirs []string
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		dir := filepath.Join(root, entry.Name())
		if info, err := os.Stat(filepath.Join(dir, "runs")); err == nil && info.IsDir() {
			dirs = append(dirs, dir)
			continue
		}
		dirs = append(dirs, dataDirsUnder(dir, depth-1)...)
	}
	return dirs
}

// sandboxOwners finds, for each sandbox name, the run under dataDirs that
// recorded it. A name no run recorded comes back with no owner.
func sandboxOwners(names, dataDirs []string) []sandboxOwner {
	owners := make([]sandboxOwner, len(names))
	index := map[string]int{}
	for i, name := range names {
		owners[i] = sandboxOwner{name: name}
		index[name] = i
	}
	unmatched := len(names)
	for _, dataDir := range dataDirs {
		if unmatched == 0 {
			break
		}
		entries, err := os.ReadDir(filepath.Join(dataDir, "runs"))
		if err != nil {
			continue
		}
		for _, entry := range entries {
			if !entry.IsDir() {
				continue
			}
			records, err := sandbox.RecordedSandboxes(dataDir, entry.Name())
			if err != nil {
				continue
			}
			for _, rec := range records {
				i, wanted := index[rec.Name]
				if !wanted || owners[i].runID != "" {
					continue
				}
				owners[i].dataDir, owners[i].runID = dataDir, entry.Name()
				owners[i].lost = sandboxRunLost(dataDir, entry.Name())
				unmatched--
			}
		}
	}
	return owners
}

// sandboxRunLost reports whether the run or request job recorded under
// runID will never remove its own sandbox: the run is confirmed terminal, or
// the factoryd process that owned it is provably gone. A drafting or
// planning job records its sandbox under its request's id and has no run
// record; its owner alone decides. A record that cannot be read is not
// lost: it is left alone.
func sandboxRunLost(dataDir, runID string) bool {
	r, err := run.Load(dataDir, runID)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return runOwnerDead(dataDir, runID)
	case err != nil:
		return false
	}
	return runTerminalConfirmed(r) || runOwnerDead(dataDir, runID)
}

// sandboxRemovable reports whether the sandbox recorded under runID may be
// deleted now: its run is confirmed terminal, or it has no run record and
// its owner is gone. A run whose owner is gone and that could not be halted
// (its workflow could not be reached) is not: it stays as it is until it
// can be.
func sandboxRemovable(dataDir, runID string) bool {
	r, err := run.Load(dataDir, runID)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return runOwnerDead(dataDir, runID)
	case err != nil:
		return false
	}
	return runTerminalConfirmed(r)
}

// doctorCheckSandboxOwners is the row for the sandboxes the gateway holds.
// One whose build is gone keeps the OpenShell stack "in use": `factoryd stop
// -all`, `uninstall` and the stack restart of `doctor -fix` all refuse while
// it exists. It warns and never fails doctor: no build is blocked by it.
func doctorCheckSandboxOwners(owners []sandboxOwner) doctorCheck {
	check := doctorCheck{Name: "every sandbox belongs to a running build", Advisory: true}
	var lost, unknown, building []string
	for _, o := range owners {
		named := fmt.Sprintf("sandbox %s (run %s in %s)", sanitize.Line(o.name), sanitize.Line(o.runID), o.dataDir)
		switch {
		case o.runID == "":
			unknown = append(unknown, "sandbox "+sanitize.Line(o.name))
		case o.lost:
			lost = append(lost, named)
		default:
			building = append(building, named)
		}
	}
	switch {
	case len(lost) > 0:
		check.Err = fmt.Errorf("%s: its build is finished or its factoryd process is gone, so nothing will remove it", strings.Join(lost, "; "))
		check.Fix = "run `factoryd doctor -fix`: it halts the run, puts its request in resume_review and removes the sandbox"
	case len(unknown) > 0:
		check.Err = fmt.Errorf("%s: no run in a data dir doctor knows (each profile's, and those within %d levels under %s) started it", strings.Join(unknown, "; "), sandboxDataDirDepth, sessionconfig.DataRoot())
		check.Fix = "if a build is running from another data dir, leave it; otherwise run `factoryd doctor -data-dir <that data dir> -fix`, or, when that data dir was deleted, `factoryd doctor -fix -remove-sandbox <name>`"
	case len(building) > 0:
		check.Detail = strings.Join(building, "; ")
	}
	return check
}

// reclaimLostSandboxes is the -fix of doctorCheckSandboxOwners. For each
// data dir with a lost sandbox it takes that data dir's worker lock and does
// what a worker does at start: halts the runs whose owner is gone and puts
// their requests in resume_review (reclaimLostWork), ends those requests'
// workflows so none undoes the operator's next decision, and removes the
// containers of finished runs. It then deletes each lost sandbox that may be
// deleted (sandboxRemovable) through the gateway, and says what it did and
// what it left. A data dir whose lock cannot be taken is left as it is: a
// worker running there holds it.
func reclaimLostSandboxes(dp *deps, ctx context.Context, in doctorInputs, owners []sandboxOwner, w io.Writer) {
	var dirs []string
	lost := map[string][]sandboxOwner{}
	for _, o := range owners {
		if !o.lost {
			continue
		}
		if _, seen := lost[o.dataDir]; !seen {
			dirs = append(dirs, o.dataDir)
		}
		lost[o.dataDir] = append(lost[o.dataDir], o)
	}
	for _, dataDir := range dirs {
		unlock, err := acquireWorkerLock(dp, dataDir)
		if err != nil {
			fmt.Fprintf(w, "sandboxes: left %s as it is: %v\n", dataDir, err)
			continue
		}
		runs, requests := reclaimLostWork(dp, ctx, dataDir, in.sandboxDocker, in.temporalAddress)
		endRequestWorkflows(dp, ctx, in.temporalAddress, requests, w)
		if in.sandboxDocker != "" {
			removeFinishedRunContainers(ctx, in.sandboxDocker, dataDir)
		}
		removed := 0
		for _, o := range lost[dataDir] {
			if !sandboxRemovable(dataDir, o.runID) {
				fmt.Fprintf(w, "sandboxes: left sandbox %s: run %s could not be halted (the lines above say why)\n", sanitize.Line(o.name), sanitize.Line(o.runID))
				continue
			}
			if err := deleteSandbox(dp, ctx, o.name); err != nil {
				fmt.Fprintf(w, "sandboxes: could not delete sandbox %s: %v\n", sanitize.Line(o.name), err)
				continue
			}
			removed++
		}
		unlock()
		fmt.Fprintf(w, "sandboxes: %s: halted %d run(s), put %d request(s) in resume_review, deleted %d sandbox(es)\n", dataDir, len(runs), len(requests), removed)
	}
}

// deleteSandbox deletes one sandbox through the gateway, which also removes
// its containers. The binary the integration tests build has no gateway
// runtime, and the container sweep before this call is all it has.
func deleteSandbox(dp *deps, ctx context.Context, name string) error {
	runtime := dp.sandbox.runtime()
	if runtime == nil {
		return nil
	}
	return runtime.Delete(ctx, name)
}

// endRequestWorkflows ends the RequestWorkflow of each request doctor moved
// to resume_review. Its old workflow still waits on the lost step, and would
// otherwise move the request back there after the operator resumes it. A
// Temporal that cannot be reached is not fatal: the operator is told the
// command that ends each one, because no later worker start does.
func endRequestWorkflows(dp *deps, ctx context.Context, temporalAddress string, requestIDs []string, w io.Writer) {
	if len(requestIDs) == 0 {
		return
	}
	address, err := hostcontrol.ResolveTemporalAddress(dp, ctx, temporalAddress, w)
	if err == nil {
		var terminator workflowTerminator
		var closeFn func()
		if terminator, closeFn, err = dp.temporal.dialTerminator(ctx, address); err == nil {
			defer closeFn()
			terminateRequestWorkflows(ctx, terminator, requestIDs)
			return
		}
	}
	for _, id := range requestIDs {
		fmt.Fprintf(w, "sandboxes: could not end the workflow of request %s (%v): before `factoryd resume`, end it with `temporal workflow terminate --workflow-id %s`\n", sanitize.Line(id), err, workflow.RequestWorkflowID(id))
	}
}

// doctorSandboxOwnersCheck lists the gateway's sandboxes, reclaims the lost
// ones first when fix is set, and returns the row. ok is false when the
// gateway does not answer or cannot list: the gateway's own row says why.
func doctorSandboxOwnersCheck(dp *deps, ctx context.Context, in doctorInputs, fix bool, w io.Writer) (check doctorCheck, ok bool) {
	list := func() ([]sandboxOwner, error) {
		names, err := dp.sandbox.sandboxNames(ctx)
		if err != nil || len(names) == 0 {
			return nil, err
		}
		return sandboxOwners(names, sandboxDataDirs(in.dataDir)), nil
	}
	if dp.sandbox.gatewayHealthy(ctx) != nil {
		return doctorCheck{}, false
	}
	owners, err := list()
	if err != nil {
		return doctorCheck{}, false
	}
	if fix && anyLost(owners) {
		reclaimLostSandboxes(dp, ctx, in, owners, w)
		if owners, err = list(); err != nil {
			return doctorCheck{}, false
		}
	}
	switch {
	case in.removeSandbox != "" && !fix:
		fmt.Fprintf(w, "sandboxes: left sandbox %s: -remove-sandbox deletes it only with -fix\n", sanitize.Line(in.removeSandbox))
	case in.removeSandbox != "":
		removeUnownedSandbox(dp, ctx, owners, in.removeSandbox, w)
		if owners, err = list(); err != nil {
			return doctorCheck{}, false
		}
	}
	return doctorCheckSandboxOwners(owners), true
}

// doctorRegisterRemoveSandboxFlag registers -remove-sandbox directly on
// flags, as doctorRegisterYesFlag does, for a flag only
// doctorSandboxOwnersCheck reads.
func doctorRegisterRemoveSandboxFlag(flags *flag.FlagSet) *string {
	return flags.String("remove-sandbox", "", "With -fix, delete this sandbox if no run in a data dir doctor knows started it: for one left by a build whose data dir was deleted.")
}

// removeUnownedSandbox is -fix -remove-sandbox: it deletes the named sandbox
// only when no run in any data dir doctor knows recorded it. Such a sandbox
// is never lost (sandboxOwners), so reclaimLostSandboxes leaves it, and
// nothing proves its build is gone: the operator naming it is the proof. A
// sandbox a known run recorded is refused, whatever that run's state: the
// lost-sandbox reclaim, under its data dir's worker lock, is its only path.
func removeUnownedSandbox(dp *deps, ctx context.Context, owners []sandboxOwner, name string, w io.Writer) {
	i := slices.IndexFunc(owners, func(o sandboxOwner) bool { return o.name == name })
	switch {
	case i < 0:
		fmt.Fprintf(w, "sandboxes: the gateway holds no sandbox %s\n", sanitize.Line(name))
	case owners[i].runID != "":
		fmt.Fprintf(w, "sandboxes: left sandbox %s: it belongs to run %s in %s, and `factoryd doctor -fix` removes it once that build is gone\n", sanitize.Line(name), sanitize.Line(owners[i].runID), owners[i].dataDir)
	default:
		if err := deleteSandbox(dp, ctx, name); err != nil {
			fmt.Fprintf(w, "sandboxes: could not delete sandbox %s: %v\n", sanitize.Line(name), err)
			return
		}
		fmt.Fprintf(w, "sandboxes: deleted sandbox %s, which no run in a data dir doctor knows started\n", sanitize.Line(name))
	}
}

func anyLost(owners []sandboxOwner) bool {
	for _, o := range owners {
		if o.lost {
			return true
		}
	}
	return false
}
