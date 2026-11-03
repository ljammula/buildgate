package requestdriver

import (
	"log"
	"time"

	"buildgate/internal/request"
)

// recordStepWait records in *slot (r.StackedWait or r.RouteWait) why r's next
// step has not started, keeping the time the wait began, and reminds the
// operator once the wait has lasted the reminder interval since it began or
// since the last reminder (stepWaitReminderDue), as at a review gate.
func recordStepWait(dataDir string, r *request.Request, slot **request.StepWait, reason string, interval time.Duration, now time.Time) error {
	since := now.UTC().Format(time.RFC3339Nano)
	if *slot != nil && (*slot).Since != "" {
		since = (*slot).Since
	} else {
		// A new wait: reminders count from its start, not from a reminder
		// sent at an earlier gate. Only the wait that held this pass is
		// current: a hold checked earlier in the pass stops the later ones
		// being checked, so another wait left set would be stale.
		clearOtherStepWaits(r, slot)
		r.ClearReminderState()
	}
	if *slot == nil || (*slot).On != reason {
		log.Printf("request %s: %s", r.ID, reason)
	}
	*slot = &request.StepWait{Since: since, On: reason}
	if stepWaitReminderDue(r, *slot, interval, now) {
		RemindRequest(dataDir, r, now)
	}
	return r.Save(dataDir)
}

// stepWaitReminderDue reports whether wait is due a reminder at now: interval
// has passed since the wait began, and since r's last reminder. An interval
// of 0 or less reminds never.
func stepWaitReminderDue(r *request.Request, wait *request.StepWait, interval time.Duration, now time.Time) bool {
	if interval <= 0 || wait == nil {
		return false
	}
	from, err := time.Parse(time.RFC3339Nano, wait.Since)
	if err != nil {
		return false
	}
	if last, err := time.Parse(time.RFC3339Nano, r.LastNotifiedAt); err == nil && last.After(from) {
		from = last
	}
	return now.Sub(from) >= interval
}

// RecordRouteWait records that r's next step waits for its model route,
// reason saying which route, why, and what to do (request.Request.RouteWait).
func RecordRouteWait(dataDir string, r *request.Request, reason string, interval time.Duration, now time.Time) error {
	return recordStepWait(dataDir, r, &r.RouteWait, reason, interval, now)
}

// EndRouteWait clears r's route wait, and the reminders that came with it,
// once the route can be used.
func EndRouteWait(dataDir string, r *request.Request) error {
	if r.RouteWait == nil {
		return nil
	}
	endStepWait(r, &r.RouteWait)
	return r.Save(dataDir)
}

// stepWaits are r's step-wait slots.
func stepWaits(r *request.Request) []**request.StepWait {
	return []**request.StepWait{&r.StackedWait, &r.RouteWait}
}

// endStepWait clears *slot and, unless another step wait is still set, the
// reminders that came with it.
func endStepWait(r *request.Request, slot **request.StepWait) {
	if *slot == nil {
		return
	}
	*slot = nil
	for _, w := range stepWaits(r) {
		if *w != nil {
			return
		}
	}
	r.ClearReminderState()
}

// clearOtherStepWaits clears every step wait of r but *slot.
func clearOtherStepWaits(r *request.Request, slot **request.StepWait) {
	for _, w := range stepWaits(r) {
		if w != slot {
			*w = nil
		}
	}
}
