package hostcontrol

import (
	"context"
	"fmt"
	"time"

	"buildgate/internal/sanitize"
)

// ActiveBuilds names what is using the OpenShell stack: each request a worker
// of dataDirs reports as building, and each sandbox the gateway lists. An
// error means the sandboxes could not be listed while the gateway answers, so
// whether a build is running is unknown; with no gateway running there are no
// sandboxes and a list failure is not an error.
func ActiveBuilds(dp Deps, ctx context.Context, dataDirs []string, now time.Time) ([]string, error) {
	var active []string
	for _, dir := range dataDirs {
		for _, id := range ActiveRequests(dp, dir, now) {
			active = append(active, "request "+sanitize.Line(id))
		}
	}
	names, err := dp.SandboxNames(ctx)
	if err != nil {
		if dp.GatewayHealthy(ctx) != nil {
			return active, nil
		}
		return active, fmt.Errorf("list sandboxes: %w", err)
	}
	for _, name := range names {
		active = append(active, "sandbox "+sanitize.Line(name))
	}
	return active, nil
}
