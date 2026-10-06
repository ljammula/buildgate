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
```

## Module layout

```text
src/
  domain/     pure TypeScript: types, decoders, formatting and rules. No React, no I/O
  api/        the only code that talks HTTP: one file per resource, query hooks, SSE
  platform/   the only code that touches browser globals (storage, location, title)
  routes/     the route table and typed link builders: every path is written here once
  ui/         shared presentational components. No data fetching
  features/   one folder per screen area: its screen, local components, hooks, tests
  app/        App, router wiring, providers
  test/       test setup and fixture readers
```

```text
app ──> features ──> ui ──> domain
           │
           ├──> api ──────> domain
           ├──> routes ───> domain
           └──> platform ─> domain
features/X never imports features/Y (navigate with a routes/ link builder)
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

## Fixtures shared with Go

`../console/test/fixtures/` is read by Go, Dart and these tests (`src/test/fixtures.ts`):

| Directory  | Written by                                                                         | What it pins                                                                                                                             |
| ---------- | ---------------------------------------------------------------------------------- | ---------------------------------------------------------------------------------------------------------------------------------------- |
| `api/`     | `FACTORYD_UPDATE_GOLDEN=1 go test ./internal/api -run TestConsoleContractFixtures` | The response of every read route, from a fixed data directory. `index.json` maps each file to its route. Every decoder decodes its files |
| `vectors/` | By hand                                                                            | Inputs and expected outputs for content hashing and token/cost formatting, which the server and the console must compute identically     |
