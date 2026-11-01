# Spec

## Problem

The board app needs a browser client for the backend of `docs/api.md`:
sign up and log in, see and create boards, invite members, work with
columns and cards (add, edit, assign, move, delete), and see every other
member's changes live. The repository has the wire types
(`frontend/src/api/types.ts`), an empty Vite + React + Vitest app and the
API contract; nothing else of the frontend exists.

## Scope

- `src/api/`: the REST client and the WebSocket client.
- `src/auth/`: token storage, the auth context, the log in / sign up screen.
- `src/route.ts`: hash routes.
- `src/board/`: board state (the version rule, move targets), the board view
  and the card editor.
- `src/boards/`: the board list and the new-board form.
- `src/live.tsx`: one WebSocket per signed-in session shared by the screens.
- `src/App.tsx`: the signed-in shell and route switch.

## Non-goals

- Drag and drop (cards move with buttons).
- Offline use, optimistic updates, undo.
- Renaming boards or columns in the UI (the API supports it; the UI does not
  offer it).
- Styling beyond a usable layout: columns side by side, cards stacked.
- Any change to `docs/api.md`, `src/api/types.ts`'s existing declarations, or
  the backend.

## Affected services and packages

- `frontend/src/api` (new code beside `types.ts`)
- `frontend/src/auth`, `frontend/src/board`, `frontend/src/boards` (new)
- `frontend/src/route.ts`, `frontend/src/live.tsx` (new)
- `frontend/src/App.tsx`, `frontend/src/App.test.tsx` (replace the placeholders)
- `frontend/src/styles.css`, `frontend/src/main.tsx` (import the stylesheet)

## Acceptance criteria

1. `createApi(getToken)` in `src/api/client.ts` has one function per REST route of `docs/api.md`, sends `Authorization: Bearer <token>` when `getToken()` returns one, sends JSON bodies with `Content-Type: application/json`, and resolves a 204 to `undefined`.
2. A non-2xx response with the error body rejects with `ApiError` carrying `status`, `code` and `message` from the body; a non-2xx response without that body rejects with `ApiError(status, "internal", "Something went wrong")`; a failed `fetch`, or a failure while reading a 2xx body, rejects with `ApiError(0, "network", "Cannot reach the server")`.
3. `src/auth/token.ts` keeps the session token in `localStorage` under the key `board.token` (`loadToken`, `saveToken`, `clearToken`), and returns `null` when storage is empty or throws.
4. `connectSocket` in `src/api/socket.ts` opens `ws:` (or `wss:` on an `https:` page) `/api/ws?token=<url-encoded token>`, passes each parsed `board` and `boards.changed` event to its callback, ignores any other frame, reports `"open"` and `"closed"` status changes, reconnects after an unexpected close after 1 s, 2 s, 4 s, 8 s, then every 10 s (reset to 1 s after a successful open), and stops for good after `close()`.
5. `subscribe(boardId)` on the socket sends `{"type":"subscribe","board_id":<id>}` at once when open, and the most recent subscription is sent again after every reconnect; `subscribe(null)` forgets it.
6. `parseRoute(hash)` in `src/route.ts` returns `{name: "board", id}` for `#/boards/<id>` with `id` a positive integer and `{name: "boards"}` for anything else; `boardHref(id)` returns `#/boards/<id>`.
7. `newerBoard(current, incoming)` in `src/board/state.ts` returns `incoming` when `current` is null or has the same `id` and a lower `version`, and otherwise returns `current`; `moveTargets(board, cardId)` returns `left`, `right`, `up` and `down` as `{column_id, position}` or `null`: left/right are the previous/next column at the card's current position (null at the first/last column), up/down are the same column at position - 1 / + 1 (null at the top/bottom).
8. The auth screen has a `Boards` heading, `Log in` / `Sign up` mode buttons, labelled `Username`, `Display name` (sign up only) and `Password` inputs, a submit button `Log in` / `Create account` disabled while the request runs, and shows an `ApiError`'s message (any other error: `Something went wrong`) in a `role="alert"` paragraph; a stored token is checked with `GET /api/me` on load (401 clears it; any other failure keeps it and shows the auth screen).
9. Signed in, the app shows `Signed in as <display_name>` and a `Log out` button (which clears the token even when the logout request fails), and shows the board list at `#/` and the board at `#/boards/<id>`, following `hashchange`.
10. The board list shows each board's name as a link to its board, `No boards yet` when there are none, and a form with a labelled `Board name` input and a `Create board` button that creates the board and navigates to it, showing a validation message in a `role="alert"` paragraph; it reloads when the socket delivers `boards.changed`.
11. The board view shows the board name as a heading, a `Back to boards` link, each column as a `region` named by the column's name with its cards in position order, an `Add card` form per column (input labelled `New card in <column name>`, button `Add`), an `Add column` form (input labelled `Column name`, button `Add column`), a `Delete column <name>` button per column (a 409 shows `column has cards` in an alert), the members' display names, and an invite form (input labelled `Invite by username`, button `Invite`; a 404 shows `No user with that username`).
12. The board view subscribes the socket to its board while open (and unsubscribes when it closes), shows every `board` event and every REST response for that board through `newerBoard`, reloads the board with `GET /api/boards/{id}` after each successful change it makes, shows `Board not found` with the back link for a 404, and shows `Reconnecting…` in a `role="status"` element while a socket that was open is closed.
13. Each card shows its title and its assignee's display name (none when unassigned) and buttons `Move <title> left`, `right`, `up`, `down` (accessible names exactly `Move <title> left` etc.) that call `POST /api/cards/{id}/move` with the target from `moveTargets` and are disabled when the target is `null`.
14. Activating a card's title opens a `dialog` named `Edit card` with `Title` and `Description` inputs and an `Assignee` select (`Unassigned` plus every member by display name); `Save` sends `PATCH /api/cards/{id}` with only the fields that changed (`assignee_id: null` for `Unassigned`) and closes on success, a validation error stays open with the message in an alert; `Delete card` deletes it and closes; `Cancel` closes without a request.
15. `npm run check` (typecheck and Vitest, run with `TZ=Pacific/Auckland`) passes, every component test drives the UI through roles and labels with `@testing-library/user-event`, and every network call in tests goes through a stubbed `fetch` or a fake `WebSocket`.

## Risks

- jsdom has no WebSocket server: tests use a hand-written fake `WebSocket`
  class installed with `vi.stubGlobal`.
- A REST response and a socket event for one board can arrive in either
  order; showing them through `newerBoard` keeps the newest.
- Host Node 26 ships an experimental `localStorage` that shadows jsdom's;
  the sandbox runs Node 22. Token tests must pass on both (developers run
  `NODE_OPTIONS=--no-experimental-webstorage` on Node 26).

## Open questions

None.
