Verify-Command: python -m pip install -q -e . pytest && python -m pytest -q tests
Allowed-Files: src/click/globals.py, src/click/utils.py, src/click/termui.py, docs/utils.md, tests/test_utils/test_echo.py
Required-Changed-Files: src/click/globals.py, src/click/utils.py, src/click/termui.py, docs/utils.md, tests/test_utils/test_echo.py

## Goal

Make `FORCE_COLOR` and `NO_COLOR` dynamic defaults for Click's `echo` and `secho` output while preserving explicit call and context color settings, existing TTY autodetection, public signatures, and the current stdout/stderr stream handling.

## Plan

### Files to touch

- `src/click/globals.py`
- `src/click/utils.py`
- `src/click/termui.py`
- `docs/utils.md`
- `tests/test_utils/test_echo.py`

### Steps

1. Extend the internal color-default resolution used by `echo` so an explicit `color` argument wins first, an explicit color value on the active Click context wins second, and only an unresolved default consults the process environment. Check `NO_COLOR` before `FORCE_COLOR`, treat only non-empty values as active, and return an unresolved value when both are inactive so the existing stream/TTY detection remains the fallback. Read `os.environ` during each resolution rather than caching the result.
2. Keep `click.echo` and `click.secho` signatures and delegation unchanged, update their API documentation to describe the environment defaults and precedence, and ensure the resolved preference is applied equally after selecting stdout or stderr.
3. Expand the echo tests with styled `echo` and `secho` cases for redirected stdout and stderr under forced color, TTY stdout and stderr under disabled color, both variables active, unset and empty variables, and the existing autodetection fallback.
4. Add coverage that explicit call-level `color=True`/`False` and explicit context-level `color=True`/`False` override either environment request, and that changing environment variables between successive calls changes only later default decisions.
5. Document the `FORCE_COLOR` and `NO_COLOR` semantics in the ANSI Colors section of the utilities guide, including non-empty activation, empty-value inactivity, `NO_COLOR` precedence, and explicit call/context precedence.

### Tests to add

- Add parametrized stdout/stderr tests for `click.echo` and `click.secho` that assert ANSI sequences are preserved or stripped under `FORCE_COLOR`, `NO_COLOR`, both variables, empty values, and an environment with neither variable.
- Exercise redirected non-TTY streams and simulated color-capable TTY streams independently for both `err=False` and `err=True`.
- Verify explicit call colors and active context colors override environment defaults in both directions.
- Mutate, clear, and reassign the environment between output calls to verify resolution is not cached.

### Acceptance criteria covered

- 1
- 2
- 3
- 4
- 5
- 6
- 7
- 8
- 9
- 10

## Out of scope

- Adding a Click option, context attribute, or programmatic configuration API for either variable.
- Parsing environment values as booleans or supporting color variables other than `FORCE_COLOR` and `NO_COLOR`.
- Changing `click.style`, `click.unstyle`, byte output, direct `print` behavior, ANSI capability detection, or public call signatures.
- Changing color policy independently for unrelated APIs or changing the existing fallback behavior when no environment default is active.
