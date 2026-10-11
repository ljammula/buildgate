# Spec

## Problem

`Group.list_commands` in `src/click/core.py` always returns the subcommand names sorted alphabetically, so the `Commands:` section of a group's help lists them alphabetically too. An application whose commands have a natural order, such as `init`, `build`, `deploy`, cannot show them in the order they were registered without subclassing `Group` and overriding `list_commands`. `CommandCollection.list_commands` has the same limit. Existing applications need the alphabetical order to stay the default.

## Scope

Add a `sort_commands: bool = True` argument to `Group.__init__` in `src/click/core.py`, stored as the `sort_commands` attribute. When it is `True`, `Group.list_commands` returns the names sorted as it does today. When it is `False`, `Group.list_commands` returns the names in the iteration order of `Group.commands`, which is the order the commands were registered in. `CommandCollection`, which passes its keyword arguments to `Group`, honours the same attribute in its own `list_commands`: sorted and de-duplicated as today when `True`; when `False`, its own commands first, then each source's commands in source order, each name listed once at its first position.

The help output and everything else that reads `list_commands` follow the returned order with no further change. `@click.group(sort_commands=False)` works through the existing keyword pass-through in `src/click/decorators.py`, which needs no edit.

The scope also includes documenting the argument in the `Group` class docstring with a `:param sort_commands:` line and a `.. versionchanged:: 8.6.0` note, adding tests to `tests/test_commands.py`, and adding one entry to `CHANGES.md` as the last bullet of the `## Version 8.6.0` list, written in the style of the entries above it and without a `{pr}` or `{issue}` role, since the number is not known when the change is built.

## Non-goals

- Changing the default order: a group created without `sort_commands` lists its commands alphabetically, as now.
- Adding a custom sort key, a reverse order, command sections, or any other way of ordering commands.
- Changing `Group.to_info_dict`, `Group.format_commands`, `Group.add_command`, shell completion code, or `src/click/decorators.py`.
- Changing the order of options or arguments in help output.
- Editing any file under `docs/`.

## Affected services and packages

The `click` Python package is affected through the public `Group` and `CommandCollection` classes in `src/click/core.py`, with tests in `tests/test_commands.py` and one line in `CHANGES.md`. No external service, persistent data, or runtime dependency is affected.

## Acceptance criteria

1. `Group.__init__` in `src/click/core.py` accepts a `sort_commands` argument annotated as `bool` with default value `True`, placed after `result_callback` and before `**kwargs`, and stores it unchanged as the instance attribute `sort_commands`, so `click.Group("cli").sort_commands` is `True` and `click.Group("cli", sort_commands=False).sort_commands` is `False`.
2. `Group.list_commands` in `src/click/core.py`, for a group created without `sort_commands` or with `sort_commands=True` to which commands named `zeta`, `alpha` and `mid` were added in that order with `add_command`, returns exactly `["alpha", "mid", "zeta"]`.
3. `Group.list_commands` in `src/click/core.py`, for the same three commands added in the same order to a group created with `sort_commands=False`, returns exactly `["zeta", "alpha", "mid"]`.
4. `Group.list_commands` in `src/click/core.py`, for `click.Group("cli", [click.Command("b"), click.Command("a")], sort_commands=False)`, returns exactly `["b", "a"]`.
5. For a group declared with `@click.group(sort_commands=False)` on a function named `cli`, with commands `zeta`, `alpha` and `mid` declared in that order with `@cli.command()` and the docstrings `Z.`, `A.` and `M.`, `Group.format_help` in `src/click/core.py` makes `cli --help` exit with code 0 and print exactly:

   ```text
   Usage: cli [OPTIONS] COMMAND [ARGS]...

   Options:
     --help  Show this message and exit.

   Commands:
     zeta   Z.
     alpha  A.
     mid    M.
   ```

6. `CommandCollection.__init__` in `src/click/core.py` accepts `sort_commands` as a keyword argument through its existing `**kwargs` pass-through to `Group.__init__`, so `click.CommandCollection("cli").sort_commands` is `True` and `click.CommandCollection("cli", sort_commands=False).sort_commands` is `False`.
7. `CommandCollection.list_commands` in `src/click/core.py`, when the collection's `sort_commands` is `True`, returns the names of its own commands and of every source's `list_commands(ctx)`, each once, sorted alphabetically, whatever the `sort_commands` value of each source. For a collection with own commands `own`, `aa`, a first source created with `sort_commands=False` holding `zz`, `own`, `bb` in that order, and a second source with default settings holding `yy`, `bb`, `cc`, it returns exactly `["aa", "bb", "cc", "own", "yy", "zz"]`.
8. `CommandCollection.list_commands` in `src/click/core.py`, when the collection's `sort_commands` is `False`, returns its own commands in registration order, followed by the result of each source's `list_commands(ctx)` in the order of `CommandCollection.sources`, leaving out a name that already appeared. Each source's own `sort_commands` decides the order of that source's names. For the collection of criterion 7 created with `sort_commands=False`, it returns exactly `["own", "aa", "zz", "bb", "cc", "yy"]`.
9. The `Group` class docstring in `src/click/core.py` has a `:param sort_commands:` line that says commands are listed alphabetically when it is `True`, in registration order when it is `False`, and that the default is `True`, and a `.. versionchanged:: 8.6.0` note that says the `sort_commands` parameter was added.
10. `tests/test_commands.py` has tests that assert the exact results of criteria 2, 3, 5, 7 and 8, and `CHANGES.md` has one new entry naming `sort_commands` as the last bullet of the `## Version 8.6.0` list, directly above the `## Version 8.5.1` heading.
11. `python -m pytest -q tests -k "not pager"` passes.

## Risks

- Backward compatibility depends on `True` staying the default and on the `True` path returning the same sorted list as before. Applications and tests that compare help output regress if the default order changes.
- `Group.commands` may be any mutable mapping supplied by the caller. Registration order is the mapping's iteration order, so a caller that passes a mapping with another iteration order gets that order when `sort_commands` is `False`.
- A subclass that overrides `list_commands` without reading `sort_commands` ignores the argument. That is the existing override contract and is left as it is.
- `CHANGES.md` is edited by every change of the release. Adding the entry as the last bullet of the `## Version 8.6.0` list keeps it apart from entries other open changes add at the top of that list.

## Open questions

None.
