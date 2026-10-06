package main

import "buildgate/internal/consolelink"

// consoleRunURL returns the console's deep link to run id under base (as
// returned by resolveConsoleBaseURL), matching the path-based routing
// console/lib/request_board_route.dart serves -- /runs/{id} loads that
// run directly, no client-side navigation needed. Mirrors
// consoleRequestURL (console_link.go) for the run-scoped route; kept in
// its own file since console_link.go belongs to another work package.
// Returns "" when base is "", so callers skip linking anything rather
// than a broken partial URL.
func consoleRunURL(base, id string) string {
	return consolelink.RunURL(base, id)
}
