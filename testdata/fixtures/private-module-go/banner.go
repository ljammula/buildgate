// Package greet is a proving-ground target whose one dependency is a module
// no public proxy has (private.example/acme/shout): a build of it passes only
// when the sandbox is given that module.
package greet

import "private.example/acme/shout"

// Banner is the fixed banner line.
func Banner() string {
	return shout.Loud("buildgate")
}
