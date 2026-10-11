# Spec

## Problem

Cobra has no way to visit every command of a command tree. `Command.VisitParents` in `command.go` walks upwards only, so tools that need the whole tree (documentation generators, linters that check every command has a `Short`, programs that list all command paths) each write their own recursion over `Command.Commands()`, and none of them can prune a subtree or stop early in a uniform way.

## Scope

Add a downward tree walk to `command.go`: an exported method `Walk` on `*Command` and an exported sentinel error `ErrSkipCommand`. `Walk` calls a caller-supplied function for the receiver and then for every command below it, depth first and parents before children, in the order `Command.Commands()` returns subcommands. The function can return `ErrSkipCommand` to skip the subcommands of the command it was called with, or any other error to stop the walk and have that error returned.

The scope also includes a doc comment on `Walk` and on `ErrSkipCommand`, and tests for the new behaviour in `command_test.go`.

## Non-goals

- Changing `Command.VisitParents`, `Command.Commands`, `Command.Find`, `Command.Traverse` or any other existing function.
- Filtering hidden, deprecated or non-runnable commands; `Walk` visits every command and the callback decides.
- Adding the default `help` or `completion` commands to a tree as a side effect of walking it.
- Using `Walk` inside Cobra's own completion, help or `doc/` generator code.
- Changing `README.md`, `site/content/user_guide.md` or any file other than `command.go` and `command_test.go`.

## Affected services and packages

The root `cobra` Go package (`github.com/spf13/cobra`) is affected through two new exported identifiers in `command.go`, with tests in `command_test.go`. The `doc` package, external services, persistent data and module dependencies (`go.mod`, `go.sum`) are not affected.

## Acceptance criteria

1. `command.go` declares the exported package-level variable `ErrSkipCommand` of type `error`, created with `errors.New`, and `ErrSkipCommand.Error()` returns exactly `"skip this command"`.
2. `command.go` declares the method `Walk` on `*Command` with exactly the signature `func (c *Command) Walk(fn func(cmd *Command) error) error`.
3. `Walk` in `command.go` calls `fn` exactly once for the receiver `c` first, then walks each subcommand in the order of the slice returned by `c.Commands()`, visiting each subcommand and all of its descendants before the next subcommand (depth first, pre-order). For a tree built with `root.AddCommand(zeta, alpha, mid)`, `zeta.AddCommand(zetaB, zetaA)`, `alpha.AddCommand(alphaA)` and `alphaA.AddCommand(alphaAX)` (names `root`, `zeta`, `zeta-b`, `zeta-a`, `alpha`, `alpha-a`, `alpha-a-x`, `mid`), `root.Walk` visits, with `EnableCommandSorting` set to `true`, exactly `root`, `alpha`, `alpha-a`, `alpha-a-x`, `mid`, `zeta`, `zeta-a`, `zeta-b`, and with `EnableCommandSorting` set to `false`, exactly `root`, `zeta`, `zeta-b`, `zeta-a`, `alpha`, `alpha-a`, `alpha-a-x`, `mid`.
4. `Walk` in `command.go` starts at its receiver and never visits the receiver's parents or siblings: in the tree of criterion 3, `alpha.Walk` visits exactly `alpha`, `alpha-a`, `alpha-a-x`, and `Walk` on a command with no subcommands calls `fn` exactly once and returns `nil`.
5. `Walk` in `command.go` visits commands whose `Hidden` field is `true` and commands whose `Deprecated` field is non-empty exactly like any other command.
6. When `fn` returns an error for which `errors.Is(err, ErrSkipCommand)` is `true` (the sentinel itself or an error wrapping it with `%w`), `Walk` in `command.go` does not visit any descendant of the command `fn` was called with, continues with that command's next sibling, and does not return that error. In the tree of criterion 3 with sorting enabled, returning `ErrSkipCommand` for `alpha` visits exactly `root`, `alpha`, `mid`, `zeta`, `zeta-a`, `zeta-b` and `Walk` returns `nil`; returning `ErrSkipCommand` for the receiver itself visits only the receiver and `Walk` returns `nil`.
7. When `fn` returns any other non-nil error, `Walk` in `command.go` calls `fn` for no further command and returns that same error value unchanged (not wrapped). In the tree of criterion 3 with sorting enabled, an error returned for `alpha-a` leaves the visited commands at exactly `root`, `alpha`, `alpha-a`.
8. `Walk` in `command.go` returns `nil` when `fn` returns `nil` for every command, and does not add, remove or initialise commands: after `root.Walk` on the tree of criterion 3, `len(root.Commands())` is `3` and no command named `help` or `completion` has been added.
9. `Walk` and `ErrSkipCommand` in `command.go` each have a doc comment starting with the identifier's name; the comment on `Walk` states the visiting order, the effect of returning `ErrSkipCommand`, and that any other error stops the walk and is returned.
10. `command_test.go` contains tests for `Walk` covering the pre-order sequence with `EnableCommandSorting` both `true` and `false` (restoring the previous value of the global afterwards), a walk started at a subcommand, `ErrSkipCommand` on a subcommand and on the receiver, a wrapped `ErrSkipCommand`, and early stop with the callback's error returned unchanged; `go test ./...` and `make fmt` pass.

## Risks

- `Command.Commands()` sorts the subcommand slice in place the first time it is called when `EnableCommandSorting` is `true`, so the visiting order depends on that package-level variable. Tests that change it must restore it, or they will change the order seen by unrelated tests in the same package.
- `ErrSkipCommand` becomes part of the public API. Comparing with `==` instead of `errors.Is` would silently treat a wrapped sentinel as a fatal error and stop the walk.
- A callback that adds or removes subcommands of the command being walked changes the slice during iteration; this request does not define the outcome of that and the doc comment should not promise one.

## Open questions

None.
