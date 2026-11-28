package sessionconfig

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// MinAge is the youngest age prune accepts: work finished less than a day ago
// may still be read, retried or reviewed.
const MinAge = 24 * time.Hour

// ParseAge reads an age: a whole number of days such as "14d", or a Go
// duration such as "36h". It must be at least MinAge. prune_after and
// `factoryd prune -older-than` take one.
func ParseAge(s string) (time.Duration, error) {
	var d time.Duration
	if days, ok := strings.CutSuffix(s, "d"); ok {
		n, err := strconv.Atoi(days)
		if err != nil {
			return 0, fmt.Errorf("%q is not a number of days such as 14d", s)
		}
		d = time.Duration(n) * 24 * time.Hour
	} else {
		parsed, err := time.ParseDuration(s)
		if err != nil {
			return 0, fmt.Errorf("%q is not an age such as 14d or 36h", s)
		}
		d = parsed
	}
	if d < MinAge {
		return 0, fmt.Errorf("%q: an age must be at least 24h", s)
	}
	return d, nil
}

// MinVMIdleStopAfter is the shortest vm_idle_stop_after accepted: a shorter
// one would stop and start the VM over a pause between two requests.
const MinVMIdleStopAfter = 5 * time.Minute

// ParseVMIdleStopAfter reads vm_idle_stop_after: a Go duration such as "30m",
// at least MinVMIdleStopAfter, or a zero ("0", "0s") that turns it off.
func ParseVMIdleStopAfter(s string) (time.Duration, error) {
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, fmt.Errorf("%q is not a duration such as 30m", s)
	}
	if d != 0 && d < MinVMIdleStopAfter {
		return 0, fmt.Errorf("%q: must be 0 (off) or at least %s", s, MinVMIdleStopAfter)
	}
	return d, nil
}
