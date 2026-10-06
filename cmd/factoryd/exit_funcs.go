package main

// exitFuncs collects the functions a command's stages want run when the
// command returns, as a defer in one long function would run them. The
// command defers runDeferred once; a stage calls atExit where it would have
// written defer.
type exitFuncs struct {
	deferred []func()
}

// atExit registers fn to run when the command returns.
func (e *exitFuncs) atExit(fn func()) { e.deferred = append(e.deferred, fn) }

// runDeferred runs the registered functions, last registered first.
func (e *exitFuncs) runDeferred() {
	for i := len(e.deferred) - 1; i >= 0; i-- {
		e.deferred[i]()
	}
}
