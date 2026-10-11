# Spec

## Problem

Cobra's positional-argument validation API exposes `MatchAll(pargs ...PositionalArgs)` in `args.go`, which requires every supplied validator to pass. It cannot express alternatives, so commands accepting multiple valid argument shapes must implement custom validators. For example, a command accepting either zero arguments or exactly two cannot currently compose `NoArgs` and `ExactArgs(2)` declaratively. The missing public counterpart is `MatchAny`.

## Scope

The `github.com/spf13/cobra` package will expose `func MatchAny(pargs ...PositionalArgs) PositionalArgs` in `args.go`, next to `MatchAll`. The returned validator will evaluate the supplied `PositionalArgs` validators in order, succeed immediately when any validator returns `nil`, return the final validator error when all validators fail, and return `nil` when no validators are supplied, matching `MatchAll`'s empty-input behavior.

The deliverable includes positional-argument coverage beside the existing `MatchAll` coverage in `args_test.go` for successful alternatives, ordered short-circuiting, the all-failing case, and no validators. The relevant argument-validation material in `site/content/user_guide.md` will name `MatchAny` wherever it lists `MatchAll` and explain the difference between requiring all validators and accepting any validator.

## Non-goals

- Changing the behavior or error handling of `MatchAll`, `NoArgs`, `ExactArgs`, or other existing positional-argument validators.
- Aggregating, wrapping, or rewriting validator errors, or changing the order in which supplied validators are considered.
- Defining new command parsing, argument normalization, or command execution behavior.
- Adding documentation outside the relevant `site/content/user_guide.md` argument-validation guidance.

## Affected services and packages

The affected code is the root Go package `github.com/spf13/cobra`, specifically its public positional-validator API consisting of `PositionalArgs`, `MatchAll`, and `MatchAny`, with the associated argument-validation surface in `args_test.go`. The user-facing documentation affected is `site/content/user_guide.md`. No external service, persisted data, or network integration is involved.

## Acceptance criteria

1. `args.go` exports `MatchAny(pargs ...PositionalArgs)` with the return type `PositionalArgs`, and the returned value can be assigned to a Cobra command's `Args` field.
2. `MatchAny` in `args.go` evaluates supplied `PositionalArgs` validators in the order provided, returns `nil` as soon as the first validator returns `nil`, and does not invoke validators that follow that successful validator.
3. When every supplied validator passed to `MatchAny` in `args.go` returns an error, the returned error is the error from the last supplied validator, without replacing it with an earlier error or a combined error.
4. The validator returned by `MatchAny()` in `args.go` returns `nil` for any command and positional-argument slice, because no validator is supplied.
5. A command using `Args: cobra.MatchAny(cobra.NoArgs, cobra.ExactArgs(2))` accepts `cmd` with no positional arguments and `cmd a b`, while `cmd a` fails with `accepts 2 arg(s), received 1`.
6. Existing `MatchAll` behavior in `args.go` remains unchanged: it requires every supplied `PositionalArgs` validator to return `nil`, returns the first failing validator's error, and returns `nil` when no validators are supplied.
7. In `site/content/user_guide.md`, every passage that lists `MatchAll` also names `MatchAny`, and the argument-validation guidance states that `MatchAll` requires all validators whereas `MatchAny` succeeds when at least one validator succeeds.

## Risks

- The exact-last-error contract depends on existing validator error text, including `ExactArgs`'s `accepts %d arg(s), received %d` message. If `MatchAny` wraps or aggregates errors, callers' CLI output changes and the documented example no longer works.
- Positional validators may have observable invocation effects. If `MatchAny` reorders validators or continues after a successful validator, later failures or side effects could alter command behavior; the ordered short-circuit contract prevents that ambiguity.
- This specification assumes the same non-nil validator input convention as `MatchAll`; it assigns no special meaning to a nil element in `pargs`. If callers require nil validators to be ignored or converted into errors, that would require a separate API decision.
- The documentation requirement depends on identifying all existing `MatchAll` listings in `site/content/user_guide.md`. An omitted listing would leave the public guidance inconsistent even if the API behavior is correct.

## Open questions

None.
