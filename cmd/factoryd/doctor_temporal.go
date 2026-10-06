package main

import (
	"context"
	"errors"
	"fmt"
	"io"

	"go.temporal.io/sdk/client"
)

// doctorCheckTemporal reports whether Temporal answers at its default
// address. Not reachable fails the check: builds run only on Temporal.
// With fix it calls temporal.ensure, which starts the embedded compose stack
// when Docker is usable and prints the outcome to w.
func doctorCheckTemporal(dp *deps, ctx context.Context, fix bool, w io.Writer) doctorCheck {
	addr := client.DefaultHostPort
	name := fmt.Sprintf("Temporal (%s)", addr)
	if dp.temporal.healthy(ctx, addr) == nil {
		return doctorCheck{Name: name}
	}
	if fix && dp.temporal.ensure(ctx, w) != "" {
		return doctorCheck{Name: name}
	}
	return doctorCheck{
		Name: name,
		Err:  errors.New("not reachable; builds need Temporal"),
		Fix:  "run `factoryd doctor -fix` to start it (needs Docker), or `make temporal-up`; FACTORYD_AUTOSTART=0 disables the automatic start",
	}
}
