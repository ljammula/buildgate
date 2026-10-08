// Package shout stands in for a module only its owner can fetch: no public
// proxy and no public repository has it. scripts/live-private-module.sh
// serves it from a file GOPROXY on the host, the way an operator's own Go
// settings reach a company's private modules.
package shout

import "strings"

// Loud is s in upper case with an exclamation mark.
func Loud(s string) string {
	return strings.ToUpper(s) + "!"
}
