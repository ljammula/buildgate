# Spec

## Problem

`github.com/spf13/cobra` help output is dependent on the order in which `AddCommand` is called. `AddCommand` records a child’s `CommandPath()` length while the child has its current parent, but `CommandPath()` includes the full ancestor chain. When a subcommand is built with help-topic children and attached to its parent afterward, the cached maximum path length remains based on the incomplete path. `CommandPathPadding()` then returns a value that is too small, so the descriptions in the default “Additional help topics” section are misaligned. Equivalent command trees built top-down and bottom-up must render the same help text.

## Scope

- Make command-path padding in `github.com/spf13/cobra` independent of `AddCommand` order, including when a fully constructed nested command subtree is attached to an ancestor later.
- Ensure `CommandPath()` and `CommandPathPadding()` reflect the command’s current complete ancestry when help topics are rendered, so all descriptions in a help-topic section align to the longest current command path.
- Preserve the existing local padding behavior of `NamePadding()` and `UsagePadding()` and the existing public command-tree API, including `RemoveCommand` recomputation behavior.
- Cover default Cobra help and usage rendering, including the “Additional help topics” section and the public padding methods used by custom templates.

## Non-goals

- Changing which commands are considered available, hidden, deprecated, runnable, or additional help topics.
- Changing command sorting, command groups, help-topic classification, minimum padding values, or unrelated help/usage formatting.
- Fixing the separate hidden/deprecated-command padding issues described in #2431 and #2426.
- Redesigning custom help templates or adding configuration for padding calculation; custom templates continue to consume the existing command and padding APIs.
- Providing new concurrency guarantees for mutating a command tree concurrently with rendering help.

## Affected services and packages

The `github.com/spf13/cobra` package, specifically command hierarchy construction through `AddCommand`, ancestry reported by `CommandPath()`, padding returned by `CommandPathPadding()`, `NamePadding()`, and `UsagePadding()`, removal through `RemoveCommand`, and the default help/usage renderer. No other service or package is in scope.

## Acceptance criteria

1. For the command tree containing `app`, `remote`, and the additional help topics `authentication` and `tls`, building the tree by calling `AddCommand` top-down or bottom-up produces byte-for-byte identical default help output. In both cases, the topic paths are `app remote authentication` and `app remote tls`, `CommandPathPadding()` for the topics is 25, and the two descriptions in “Additional help topics” begin in the same column.
2. When `AddCommand` attaches a prebuilt nested subtree to its parent after the subtree’s children were added, every affected child’s `CommandPath()` includes all attached ancestors and its `CommandPathPadding()` is large enough for the longest current child path under that parent; no stale shorter path length may determine help-topic alignment.
3. For the reproduction tree, changing only the `AddCommand` order leaves `NamePadding()` at 14 and `UsagePadding()` at 25 for the help topics, with both methods continuing to reflect local child names and uses rather than ancestor path length.
4. After `RemoveCommand` removes the child with the longest current path, the remaining commands’ `CommandPathPadding()`, `NamePadding()`, and `UsagePadding()` and the rendered “Additional help topics” section reflect only the remaining children; removing a command must not leave stale alignment or padding values.
5. Commands without an attached parent retain the existing minimum behavior of `CommandPathPadding()`, and attaching them later yields the same `CommandPathPadding()` and default help output as constructing the equivalent command tree in its final ancestor order.

## Risks

- Command paths depend on mutable parent links. If a descendant’s padding is not refreshed whenever its effective ancestry changes, bottom-up construction will continue to produce stale alignment; if refresh logic is applied inconsistently, different nesting depths may disagree.
- The default renderer and custom templates both consume the padding methods. If the correction changes `NamePadding()` or `UsagePadding()` or the established minimums, unrelated command listings may change even though the defect is limited to command paths.
- `RemoveCommand` currently establishes the expected recomputation semantics. If those semantics are not preserved, removing a command can leave deleted or longer paths influencing later help output.
- The fix relies on the existing `CommandPath()` definition and additional-help-topic selection. If either changes as part of this work, the requested alignment fix could also alter command identity or visibility.

## Open questions

None.
