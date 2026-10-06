# Buildgate console (React + TypeScript)

The operator console, being ported from the Flutter console in `../console/`.
Until the cutover this directory is not built by `make install` and not
embedded in `factoryd`; the Flutter console is the one that ships.

## Commands

```sh
npm ci                 # install (scripts are disabled by .npmrc)
npm run check          # typecheck, lint, format check, tests: the bar for every change
npm run test -- src/domain/status.test.ts   # one file
npm run dev            # dev server; set VITE_API_BASE_URL to a running factoryd
npm run build          # static bundle in dist/
test/walk/run.sh       # the live walk: a real factoryd, a real browser, every screen and action
```

| From the repository root   | What it does                                                                                                                           |
| -------------------------- | -------------------------------------------------------------------------------------------------------------------------------------- |
| `make console-react-test`  | `npm ci && npm run check`                                                                                                              |
| `make console-react-build` | Builds the bundle and embeds it in place of the Flutter one (`internal/consoleweb/dist`); the next `go build ./cmd/factoryd` serves it |

## Checks, and what each one is for

| Check                    | Run                                                                            | Catches                                                                                                   |
| ------------------------ | ------------------------------------------------------------------------------ | --------------------------------------------------------------------------------------------------------- |
| Unit and component tests | `npm run test`                                                                 | Logic and rendering, in isolation, against a fake server (`src/test/render.tsx`)                          |
| Contract fixtures        | `go test ./internal/api -run TestConsoleContractFixtures`, then `npm run test` | A Go response shape the decoders cannot read                                                              |
| Golden vectors           | The Go, Dart and Vitest suites                                                 | The server and the console disagreeing on a content hash or a token count                                 |
| Lint boundaries          | `npm run lint`                                                                 | A layer importing what it may not; HTML from untrusted text; `fetch` or `localStorage` in the wrong layer |
| Live walk                | `test/walk/run.sh`                                                             | What only a real server and browser show: 36 steps, each write checked against the server's own record    |
| Request parity           | `test/parity/run.sh`                                                           | The React console sending a different request than the Flutter console for the same action                |
| Test port map            | `node scripts/port-map.mjs`                                                    | A Flutter test with no React counterpart and no stated reason                                             |

`npx playwright install chromium` once before the walk or the parity run; `WALK_BROWSER_CHANNEL=chrome` uses an installed Google Chrome instead.

## Add a screen

1. Path: add its pattern to `routePatterns` and a builder function in `src/routes/paths.ts`, with a test. A path that is also an API read (`/projects`, `/runs/{id}/diff`) would be answered with JSON on a reload: put the screen under `/app/` or in a query string, or add the path to `consoleDeepLinkPatterns` in `internal/api/server.go`.
2. Folder: `src/features/<name>/` with `<Name>Screen.tsx` (thin: hooks and components), its components (one per file), and tests beside them.
3. Route: one `<Route>` line in `src/app/App.tsx`; a navigation entry in `src/shared/shell/AppShell.tsx` if it is a top-level screen.
4. Data: use the hooks in `src/api/*Queries.ts`. A screen never calls `fetch` or builds a query key.
5. Tests: `renderApp(<NameScreen />, { server: [...], path, pattern })`. Assert by role and name, and assert what was sent with `server.sent("POST /...")`.
6. Walk: a `step(...)` in `test/walk/walk.mjs` that opens the screen and performs each of its actions.

## Add an API route

| Step                         | Where                                                                                                                                                                                                             |
| ---------------------------- | ----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| The Go handler and its route | `internal/api/server.go` (and the operator docs the repository's `AGENTS.md` names)                                                                                                                               |
| A fixture of its response    | One line in `contractRoutes()` in `internal/api/contract_fixtures_test.go`, then `FACTORYD_UPDATE_GOLDEN=1 go test ./internal/api -run TestConsoleContractFixtures`. The test fails until every GET route has one |
| The type and decoder         | `src/domain/<resource>.ts`: `interface X` and `decodeX(o, at)`, with a test that decodes the fixture                                                                                                              |
| The call                     | `src/api/<resource>.ts`: `getX(http, ..., signal)` naming the token kind its handler checks (`read`, `start` or `override`), with a test of the exact request it sends                                            |
| The hook                     | `src/api/<resource>Queries.ts`: a query (key from `queryKeys`) or a mutation that caches the answer and invalidates what it changed                                                                               |

## Differences from the Flutter console

Behaviour is the Flutter console's unless a row below says otherwise.

| Area                          | Flutter                                                                 | React                                                                                                                                                 | Why                                                                                                |
| ----------------------------- | ----------------------------------------------------------------------- | ----------------------------------------------------------------------------------------------------------------------------------------------------- | -------------------------------------------------------------------------------------------------- |
| Look and layout               | Material                                                                | A dense, dark-first design; navigation in a sidebar                                                                                                   | Redesigned for its users, engineers. Button, link and heading names are unchanged                  |
| Text                          | Canvas: selection, find-in-page and middle-click were rebuilt by hand   | The browser's own                                                                                                                                     | Real DOM                                                                                           |
| Run diff and release decision | Pages pushed with no URL                                                | Tabs of the run page at `?view=diff` and `?view=release`                                                                                              | A reload or a shared link lands on the same view                                                   |
| Projects list, New run        | Pages pushed with no URL                                                | `/app/projects`, `/app/runs/new`                                                                                                                      | Same                                                                                               |
| Markdown links                | Label and target as plain text                                          | An `http(s)` link is clickable (`rel="noopener noreferrer nofollow"`, new tab), its target always shown beside the label; any other scheme stays text | The Flutter console had no way to open a link; the visible target is what stops a misleading label |
| A refused write               | "Request failed (N)" with the reason behind Details                     | The server's reason shown directly                                                                                                                    | The reason is the next step (found on the live walk)                                               |
| Approving                     | Hashes computed from the record passed when the sheet opened            | Same, made explicit: the dialog fixes the record it was opened with                                                                                   | A redraft arriving while the dialog is open is refused by the server, not approved unread          |
| Daemon status                 | Read `name`, `alive`, `last_heartbeat_at`, which the server never sends | Reads the server's `repository`, `state`, `pid`, `started_at`, `heartbeat_updated_at`                                                                 | A defect in the Flutter decoder                                                                    |
| Board "Disconnected"          | After 3 failed reconnects                                               | Same                                                                                                                                                  | —                                                                                                  |

## Module layout

```text
src/
  domain/     pure TypeScript: types, decoders, formatting and rules. No React, no I/O
  api/        the only code that talks HTTP: one file per resource, query hooks, SSE
  platform/   the only code that touches browser globals (storage, location, title)
  routes/     the route table and typed link builders: every path is written here once
  ui/         shared presentational components. No data fetching
  shared/     components more than one feature uses that fetch or write data
              (the approve/reject dialogs, the oracle panels, the app shell)
  features/   one folder per screen area: its screen, local components, hooks, tests
  app/        App, router wiring, providers
  test/       test setup and fixture readers
```

```text
app ──> features ──> shared ──> ui ──> domain
           │           │
           │           ├──> api ──────> domain
           │           ├──> routes ───> domain
           │           └──> platform ─> domain
           └──> (also ui, api, routes, platform directly)
features/X never imports features/Y: navigate with a routes/ link builder, and
move what two features both need into shared/
domain imports nothing outside domain
```

`eslint.config.js` enforces every arrow, so a violation fails `npm run lint`.
It also bans `dangerouslySetInnerHTML`, `innerHTML`, `eval`, `fetch` outside
`api/`, and `localStorage` outside `platform/`.

## Conventions

| Topic            | Rule                                                                                                                                                                                                                                                                               |
| ---------------- | ---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| Imports          | Across directories, through the `@/` alias (`@/domain/run`). Relative (`./x`) only inside one directory. No `../`                                                                                                                                                                  |
| Exports          | Named exports only. No default exports, no barrel `index.ts` files                                                                                                                                                                                                                 |
| Types            | `interface` with `readonly` fields for data. No `any`; `unknown` plus a decoder at the edge. No enums: a union of string literals, and `string` for a server-owned set that can grow (states)                                                                                      |
| Decoders         | One per API shape, in `domain/`, written with `domain/decode.ts`. `decodeX(o: JsonObject, at: string): X`. Required fields throw with the route name; optional fields take a fallback; unknown fields are ignored. Field names are camelCase in TypeScript, snake_case on the wire |
| Absence          | `null` for "not known" (never `undefined` in a decoded type); `""`, `0`, `false`, `[]` where Go omits a zero value                                                                                                                                                                 |
| Pure logic       | Lives in `domain/`, as functions over domain types, with a unit test beside it. Time is a parameter (`now: Date`), never read inside                                                                                                                                               |
| Components       | Function components. Props typed with an `interface XProps`. One component per file, named as the file. A screen composes hooks and components; logic over 20 lines moves to `domain/` or a hook                                                                                   |
| Data fetching    | Only through the hooks `api/<resource>.ts` exports (`useRun(id)`, `useApproveRequest()`). Query keys and invalidation live there; a screen never builds a key                                                                                                                      |
| State            | Server state in TanStack Query. Local UI state in `useState`/`useReducer`. No global store                                                                                                                                                                                         |
| Untrusted text   | Specs, logs, halt reasons and diffs are agent-written. Render as text or through `ui/Markdown`; never as HTML                                                                                                                                                                      |
| Styling          | Tailwind utility classes and the design tokens in `app/styles.css`. Variants with `class-variance-authority`; merge classes with `ui/cn`                                                                                                                                           |
| Accessible names | The same button, link and heading names as the Flutter console, so one Playwright scenario drives both                                                                                                                                                                             |
| Tests            | Beside the code as `x.test.ts(x)`. Vitest globals, Testing Library, queries by role and name. Expected values are written out, not computed by the code under test                                                                                                                 |
| Comments         | Say why. Keep a rule's origin where it came from a found bug; no history of the port                                                                                                                                                                                               |

## Visual language

The console is a dense tool for engineers: dark by default, light on request,
quiet surfaces, colour reserved for state.

| Topic        | Rule                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                     |
| ------------ | ---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| Colour       | Only the semantic tokens of `app/styles.css` (`bg-bg`, `bg-surface`, `bg-surface-raised`, `bg-surface-sunken`, `bg-surface-hover`, `border-border`, `text-fg`, `text-fg-muted`, `text-fg-subtle`, `bg-accent`, `text-accent`, `bg-accent-soft`, `text-tone-*`, `bg-tone-*-soft`, `border-tone-*-border`, `bg-diff-*`). Never a palette colour (`bg-zinc-900`), never a hex value, never a `dark:` variant: the tokens change with the theme                                                                                              |
| State colour | A request, run or PR state is drawn from `domain/status` (`statusForToken` -> `statusTone`) through `ui/tone.ts`. Nothing else picks a colour for a state                                                                                                                                                                                                                                                                                                                                                                                |
| Type         | `text-sm` (14px) body, `text-xs` for meta and table headers, `text-base`/`text-lg` `font-semibold` for headings. IDs, SHAs, paths, commands, logs, diffs and token counts are `font-mono`                                                                                                                                                                                                                                                                                                                                                |
| Density      | Controls are 28-32px high (`h-7`, `h-8`). Table rows about 36px. Card padding `p-4`; page gutter `px-6 py-5`. Gaps on the 4px scale                                                                                                                                                                                                                                                                                                                                                                                                      |
| Shape        | `rounded-md` controls, `rounded-lg` cards and dialogs, 1px `border-border`. Shadows only on popovers and dialogs (`shadow-popover`)                                                                                                                                                                                                                                                                                                                                                                                                      |
| Icons        | `lucide-react`, 16px (`size-4`), `aria-hidden` beside a text label; an icon-only button has an `aria-label`                                                                                                                                                                                                                                                                                                                                                                                                                              |
| Motion       | `transition-colors` only; no entrance animation on data                                                                                                                                                                                                                                                                                                                                                                                                                                                                                  |
| Overload     | A passing step is one line; a failure opens itself and sorts first. A block with nothing in it is absent, never "None" or "Not available". Closed detail is a `ui/Disclosure` with a one-line summary. Ages are `ui/RelativeTime` (exact time on hover); keep the exact time where it is the information (audit lines, decision history). Long machine text is `shared/request/DigestedText`: first sentence, the whole one click away. What an approval is bound to (spec, plan, oracle files in a review state) is never folded or cut |
| Focus        | Never remove the focus ring (`:focus-visible` is styled globally)                                                                                                                                                                                                                                                                                                                                                                                                                                                                        |
| Primitives   | Dialog, Tabs, Tooltip and DropdownMenu wrap the Radix primitive of the same name: it supplies focus trapping, keyboard handling and ARIA                                                                                                                                                                                                                                                                                                                                                                                                 |
| Components   | `class-variance-authority` for variants, `ui/cn` to merge a `className` prop, which every component accepts last. `ui/Button.tsx` is the pattern to copy                                                                                                                                                                                                                                                                                                                                                                                 |

## Fixtures shared with Go

`../console/test/fixtures/` is read by Go, Dart and these tests (`src/test/fixtures.ts`):

| Directory  | Written by                                                                         | What it pins                                                                                                                             |
| ---------- | ---------------------------------------------------------------------------------- | ---------------------------------------------------------------------------------------------------------------------------------------- |
| `api/`     | `FACTORYD_UPDATE_GOLDEN=1 go test ./internal/api -run TestConsoleContractFixtures` | The response of every read route, from a fixed data directory. `index.json` maps each file to its route. Every decoder decodes its files |
| `vectors/` | By hand                                                                            | Inputs and expected outputs for content hashing and token/cost formatting, which the server and the console must compute identically     |
