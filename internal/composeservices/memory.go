package composeservices

import (
	"fmt"

	units "github.com/docker/go-units"
)

// MemoryBytes parses a Docker-accepted memory amount ("512m", "2g") into
// bytes with Docker's own parser, so a comparison here matches what
// `docker run --memory` and compose's mem_limit would apply.
func MemoryBytes(amount string) (int64, error) {
	n, err := units.RAMInBytes(amount)
	if err != nil {
		return 0, fmt.Errorf("parse memory amount %q: %w", amount, err)
	}
	return n, nil
}

// EffectiveMemoryBytes is the memory limit Synthesize applies to svc: its
// own mem_limit when set and below operatorLimit, else operatorLimit.
func EffectiveMemoryBytes(svc ServiceSpec, operatorLimit string) (int64, error) {
	limit, err := MemoryBytes(operatorLimit)
	if err != nil {
		return 0, err
	}
	if svc.MemLimit > 0 && svc.MemLimit < limit {
		return svc.MemLimit, nil
	}
	return limit, nil
}
