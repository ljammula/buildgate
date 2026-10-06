package meter

import (
	"testing"
	"time"
)

func TestWindowLimiterAccumulatedOverflowForcesRefusal(t *testing.T) {
	maxInt := int(^uint(0) >> 1)
	limiter := NewWindowLimiter(maxInt, time.Minute)
	now := time.Now()

	limiter.Add(now, maxInt)
	limiter.Add(now, 1)
	if limiter.BelowLimit(now) {
		t.Fatal("belowLimit = true after accumulated overflow, want false")
	}
	if limiter.Allow(now, 1) {
		t.Fatal("allow = true after accumulated overflow, want false")
	}
}
