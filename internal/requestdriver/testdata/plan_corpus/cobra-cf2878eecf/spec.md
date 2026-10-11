# Spec

## Problem

The flag groups in `flag_groups.go` are all symmetric: `MarkFlagsRequiredTogether` requires every flag of a group once any one is set. There is no way to say that one flag needs others without the reverse being true, for example that `--token-file` needs `--user` while `--user` alone stays valid. Applications write that check by hand in `PreRunE`, with their own error wording and with no help from shell completion.

## Scope

Add a one-directional flag dependency to `flag_groups.go`: an exported method `MarkFlagDependsOn` on `*Command` that records, as a pflag annotation on the dependent flag, the flags that must also be set whenever it is set. `ValidateFlagGroups` reports a violated dependency after its three existing group checks, and `enforceFlagGroupsForCompletion` marks the dependencies of a flag that is present on the command line as required so shell completion suggests them.

The scope also includes tests in `flag_groups_test.go` and a short description with an example in the "Flag Groups" section of `site/content/user_guide.md`.

## Non-goals

- Changing the behaviour or error text of `MarkFlagsRequiredTogether`, `MarkFlagsOneRequired` or `MarkFlagsMutuallyExclusive`.
- Dependencies on a flag having a particular value; a dependency is satisfied by the flag being set to any value.
- Detecting dependency cycles; a cycle is valid and simply means the flags involved must be set together.
- Showing dependencies in help or usage output, or in the generators under `doc/`.
- Editing `completions.go`, `completions_test.go`, `command.go` or `command_test.go`; the completion change is confined to `enforceFlagGroupsForCompletion` in `flag_groups.go` and its tests go in `flag_groups_test.go`.

## Affected services and packages

The root `cobra` Go package (`github.com/spf13/cobra`) is affected through one new exported method and the flag-group validation and completion logic in `flag_groups.go`, with tests in `flag_groups_test.go` and user documentation in `site/content/user_guide.md`. The `doc` package, external services, persistent data and module dependencies (`go.mod`, `go.sum`) are not affected.

## Acceptance criteria

1. `flag_groups.go` declares the method `MarkFlagDependsOn` on `*Command` with exactly the signature `func (c *Command) MarkFlagDependsOn(flagName string, dependencies ...string)`, with a doc comment starting with `MarkFlagDependsOn`.
2. `MarkFlagDependsOn` in `flag_groups.go` calls `c.mergePersistentFlags()` first and looks flags up in `c.Flags()`, so local flags and persistent flags of the command and of its parents can be named. If `flagName` is not found it panics with exactly `fmt.Sprintf("Failed to find flag %q and mark it as being in a dependent flag group", flagName)`; this check comes first and applies even when `dependencies` is empty.
3. `MarkFlagDependsOn` in `flag_groups.go` then checks `dependencies` in the order given and panics at the first problem: a dependency equal to `flagName` panics with exactly `fmt.Sprintf("flag %q cannot depend on itself", flagName)`, and a dependency that is not found panics with exactly `fmt.Sprintf("Failed to find flag %q and mark it as being in a dependent flag group", dependency)`. A call that panics leaves no annotation behind.
4. `flag_groups.go` declares the constant `dependsOnAnnotation = "cobra_annotation_depends_on"` beside the existing annotation constants. Each successful call of `MarkFlagDependsOn` with at least one dependency appends exactly one value, `strings.Join(dependencies, " ")`, to that annotation on the flag `flagName` only; the dependency flags get no annotation. After `c.MarkFlagDependsOn("a", "c", "b")` then `c.MarkFlagDependsOn("a", "d")`, `c.Flags().Lookup("a").Annotations["cobra_annotation_depends_on"]` equals `[]string{"c b", "d"}`. A call with a known `flagName` and no dependencies adds no annotation and has no other effect.
5. `ValidateFlagGroups` in `flag_groups.go` checks dependencies after the required-together, one-required and mutually-exclusive checks, so an error from any of those three is returned in preference to a dependency error, and it still returns `nil` without checking anything when `c.DisableFlagParsing` is `true`.
6. In `ValidateFlagGroups` in `flag_groups.go`, each annotation value is one dependency group. A group is violated when its dependent flag has `Changed == true` and at least one flag named in the group has `Changed == false`; a flag explicitly set to its default value counts as set. The returned error is exactly `fmt.Errorf("if the flag %q is set the flags [%v] must also be set; missing %v", flagName, group, missing)`, where `group` is the annotation value (dependencies space-separated in the order given to `MarkFlagDependsOn`) and `missing` is a `[]string` of the unset dependency names, each name once, sorted with `sort.Strings`. With string flags `a`, `b`, `c` and `c.MarkFlagDependsOn("a", "c", "b")`, the arguments `--a x` give the error `if the flag "a" is set the flags [c b] must also be set; missing [b c]`, and `--a x --b x` give `if the flag "a" is set the flags [c b] must also be set; missing [c]`.
7. In `ValidateFlagGroups` in `flag_groups.go`, a dependency is one-directional: with `c.MarkFlagDependsOn("a", "b", "c")`, the argument sets `--b x`, `--b x --c x`, `--a x --b x --c x` and no flags at all return no error.
8. When several dependency groups are violated, `ValidateFlagGroups` in `flag_groups.go` returns the error for the dependent flag whose name sorts first alphabetically, and for that flag the first violated group in the order the `MarkFlagDependsOn` calls were made. With `MarkFlagDependsOn("d", "e")` and `MarkFlagDependsOn("b", "c")` and arguments `--d x --b x`, the error is `if the flag "b" is set the flags [c] must also be set; missing [c]`. With `MarkFlagDependsOn("a", "b")` and `MarkFlagDependsOn("a", "c", "d")` and arguments `--a x --b x --d x`, the error is `if the flag "a" is set the flags [c d] must also be set; missing [c]`. With `MarkFlagDependsOn("a", "b", "b", "c")` and arguments `--a x --c x`, the error is `if the flag "a" is set the flags [b b c] must also be set; missing [b]`.
9. `ValidateFlagGroups` in `flag_groups.go` ignores a dependency group when any flag named in it is not defined in the `Flags()` of the command being validated, matching the existing groups. With a root command that has persistent flags `ptoken` and `puser`, a local flag `local`, a subcommand `child`, and `root.MarkFlagDependsOn("ptoken", "local")`: `root child --ptoken t` returns no error and `root --ptoken t` returns `if the flag "ptoken" is set the flags [local] must also be set; missing [local]`. With `root.MarkFlagDependsOn("ptoken", "puser")` instead, `root child --ptoken t` returns `if the flag "ptoken" is set the flags [puser] must also be set; missing [puser]`.
10. `enforceFlagGroupsForCompletion` in `flag_groups.go` marks every flag of a dependency group as required, as `c.MarkFlagRequired` does, when the group's dependent flag has `Changed == true` and every flag named in the group is defined; it marks nothing for a group whose dependent flag is not set. For a subcommand `child` with string flags `token`, `user`, `org`, a bool flag `plain`, `child.MarkFlagDependsOn("token", "user", "org")` and a `ValidArgsFunction` returning `[]string{"subArg"}, ShellCompDirectiveNoFileComp`, the `ShellCompNoDescRequestCmd` completion lines before the `:4` directive line are exactly: `subArg` for `child ""`; `--org`, `--user`, `subArg` for `child --token t ""`; `--org`, `subArg` for `child --token t --user u ""`; and `subArg` for `child --user u ""`.
11. `flag_groups_test.go` contains tests for `MarkFlagDependsOn` covering a satisfied dependency, a missing dependency with the exact error text, the one-directional case, two calls on the same flag, a group ignored on a command that lacks one of its flags, the three panic messages, precedence of an existing group error over a dependency error, and the completion behaviour of criterion 10.
12. The "Flag Groups" section of `site/content/user_guide.md` describes `MarkFlagDependsOn` in one short paragraph placed before the "In these cases:" list, with a Go code block containing a call of the form `rootCmd.MarkFlagDependsOn("token-file", "user")`, and states that the dependency is one-directional.
13. `go test ./...` and `make fmt` pass, and the existing tests in `flag_groups_test.go` and `completions_test.go` pass unmodified.

## Risks

- `ValidateFlagGroups` and `enforceFlagGroupsForCompletion` run for every command execution and completion request. A mistake in how the new annotation is read could change the errors or completions of the three existing flag groups; the existing tests must keep passing unmodified.
- Error and panic strings are compared exactly by callers' tests and scripts. The dependency order inside `[...]` is the caller's order while the `missing` list is sorted; mixing the two up produces unstable or wrong messages.
- Iterating a Go map to choose which violation to report would make the reported error non-deterministic. The order in criterion 8 relies on `FlagSet.VisitAll` visiting flags in name order (pflag's default `SortFlags`) and on annotation values keeping call order.
- `enforceFlagGroupsForCompletion` mutates flag annotations through `MarkFlagRequired` during a completion request, as it already does for required-together groups; marking dependencies as required outside the completion path would make ordinary runs fail with a required-flag error instead of the dependency error.

## Open questions

None.
