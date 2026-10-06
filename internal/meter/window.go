package meter

import (
	"sync"
	"time"
)

type windowEvent struct {
	at   time.Time
	cost int
}

type WindowLimiter struct {
	mu            sync.Mutex
	limit         int
	window        time.Duration
	events        []windowEvent
	total         int
	forceExceeded bool
}

func NewWindowLimiter(limit int, window time.Duration) *WindowLimiter {
	return &WindowLimiter{limit: limit, window: window}
}

func (l *WindowLimiter) Allow(now time.Time, cost int) bool {
	l.mu.Lock()
	defer l.mu.Unlock()

	if l.forceExceeded {
		return false
	}
	l.prune(now)
	if l.total >= l.limit {
		return false
	}
	if !l.addCostLocked(cost) {
		return false
	}
	l.events = append(l.events, windowEvent{at: now, cost: cost})
	return true
}

func (l *WindowLimiter) BelowLimit(now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()

	if l.forceExceeded {
		return false
	}
	l.prune(now)
	return l.total < l.limit
}

func (l *WindowLimiter) Add(now time.Time, cost int) {
	l.mu.Lock()
	defer l.mu.Unlock()

	if l.forceExceeded || cost <= 0 {
		return
	}
	l.prune(now)
	if !l.addCostLocked(cost) {
		return
	}
	l.events = append(l.events, windowEvent{at: now, cost: cost})
}

func (l *WindowLimiter) ForceExceed() {
	l.mu.Lock()
	defer l.mu.Unlock()

	l.forceExceeded = true
}

func (l *WindowLimiter) addCostLocked(cost int) bool {
	maxInt := int(^uint(0) >> 1)
	if cost > 0 && l.total > maxInt-cost {
		l.forceExceeded = true
		return false
	}
	l.total += cost
	return true
}

func (l *WindowLimiter) prune(now time.Time) {
	cutoff := now.Add(-l.window)
	firstCurrent := 0
	for firstCurrent < len(l.events) && !l.events[firstCurrent].at.After(cutoff) {
		l.total -= l.events[firstCurrent].cost
		firstCurrent++
	}
	l.events = l.events[firstCurrent:]
}

// Total is the running total currently inside the window, as of the last
// prune (it does not prune itself).
func (l *WindowLimiter) Total() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.total
}
