# Request parity: Flutter console vs React console

Proves the React console sends the same API write requests as the Flutter console it replaces, and that the server ends up in the same state.

| Part                                       | What it does                                                                                                                                                                                                                                                                 |
| ------------------------------------------ | ---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `run.sh`                                   | Builds `factoryd-flutter` (bundle `~/buildgate/console-flutter-dist`) and `factoryd-react` (fresh `console-react/dist`) under the `internal/consoleweb/dist` lock, seeds two identical data directories, serves them on 18096 / 18097, runs `parity.mjs`, stops both servers |
| `parity.mjs`                               | Runs each scenario in both consoles through `drivers/*.mjs`, records every POST/PUT/DELETE/PATCH to the page's origin, compares                                                                                                                                              |
| `drivers/react.mjs`, `drivers/flutter.mjs` | The same exported actions; every Flutter quirk (semantics, scrolling to reveal, dialog buttons, field entry) lives in the Flutter one only                                                                                                                                   |
| `exceptions.md`                            | Scenarios whose difference a human accepted                                                                                                                                                                                                                                  |

## Run

```sh
cd console-react
test/parity/run.sh                      # all scenarios
test/parity/run.sh --only approve-spec  # some
PARITY_BUILD="npx vite build" test/parity/run.sh
```

Environment: `PARITY_DIR` (default `~/buildgate/console-parity`), `PARITY_PORT_FLUTTER`, `PARITY_PORT_REACT`, `PARITY_FLUTTER_DIST`, `PARITY_BUILD`, `WALK_BROWSER_CHANNEL=chrome`.
Output: `<PARITY_DIR>/shots/<scenario>-<console>.png` and `recording-<scenario>.json` (both recordings and outcomes).

## What is compared

| Item           | Rule                                                                                                                                                      |
| -------------- | --------------------------------------------------------------------------------------------------------------------------------------------------------- |
| Writes         | same count, method, path, response status                                                                                                                 |
| Body           | deep-equal JSON, key order ignored; only each console's own `data-*` / `workspace-*` directory is replaced by a placeholder                               |
| `content-type` | identical on both, and `application/json`                                                                                                                 |
| URLs           | none contains the start token                                                                                                                             |
| Outcome        | `GET /requests/<id>` on both: `state`, `approved_sha256`, last rejection `reason`/`by`, `spec` (plus `verify_command`, `draft_oracles` for a new request) |

## Add a scenario

Add an entry to `scenarios` in `parity.mjs` written only with driver functions. If it needs a new action, add the same function to both drivers.

## Reading a result

| Line    | Meaning                                                                                                                                                                                                       |
| ------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `SAME`  | Identical writes and outcome                                                                                                                                                                                  |
| `DIFF`  | The consoles differ: the lines say which write and key (flutter vs react). Fix the React console, or if the difference is deliberate list the scenario in `exceptions.md` with the reason (`DIFF (accepted)`) |
| `SKIP`  | The Flutter driver threw `not driven: <reason>`; counts as not identical                                                                                                                                      |
| `ERROR` | A driver failed to perform the action; see the screenshot                                                                                                                                                     |
