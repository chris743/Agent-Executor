# erp-executor API reference

The executor is a small Go HTTP service running on the RDP client machine that displays the
FAMOUS ERP as an Azure Virtual Desktop RemoteApp window. It gives an orchestrator **eyes**
(screenshots, clipboard) and **hands** (keys, typing, clicks) on that window. It contains no ERP
logic: the orchestrator decides what to do, and the executor only looks and types.

It cannot read the ERP's UI controls (the window is a remote pixel stream), so all state must be
determined from screenshots (via a vision model), the clipboard, or window titles/counts.

## Deployment facts

| Item | Value |
|---|---|
| Base URL | `http://<client-ip>:8765` (no TLS) |
| Allowed caller | `192.168.126.8` (orchestrator / GPU machine) only |
| Auth | `Authorization: Bearer <token>`; the token is in `executor-token.txt` on the client machine. Read it from the `EXECUTOR_TOKEN` env var on the orchestrator; never hard-code it. |
| ERP main window title | `KINGS RIVER PACKING, LP - FAMOUS (KRP-AVD-Workspace)` |
| Window class | `RAIL_WINDOW` (RemoteApp) |
| Main window rect (last seen) | x=0, y=0, w=1648, h=1261 physical pixels |
| Baseline dialog count | 1 was reported at idle. Verify whether this is an always-present helper window; if so, "dialog appeared" means count > 1. |

## General behavior

- **One request at a time.** All UI operations are serialized by a mutex; requests block until
  the previous one finishes. Don't fire input requests in parallel.
- **Auto-focus.** Every input request (and screenshots, unless `focus=0`) brings the ERP to the
  foreground first. If focus can't be obtained, the request fails with 409.
- **Focus guard.** Between every keystroke/chord, the executor checks that the ERP (same process)
  still has focus, that it isn't paused, and that the failsafe hasn't tripped. If any check fails,
  input stops immediately and the error says how far it got: `"... (stopped after 3 of 7 steps)"`.
- **Failsafe.** A human moving the mouse cursor into the top-left screen corner pauses the executor.
  All input then returns 423 until `POST /resume`. The ERP window sits at (0,0), so the executor
  also refuses clicks on that exact pixel.
- **Coordinates** are physical screen pixels (the executor is DPI-aware).
- **Audit trail** (if `-audit-dir` is set): a screenshot is saved ~300 ms after each action, plus a
  JSON line in `audit.log`. Typed text is never logged, only its length.
- **Timing defaults:** 60 ms after each key chord, 15 ms after each typed character.

## Errors

Errors are JSON: `{"ok": false, "error": "<message>"}`.

| Status | Meaning | Orchestrator should |
|---|---|---|
| 400 | Bad request (invalid JSON, unknown key name, click outside ERP windows, bad parameter) | Fix the request; this is a bug |
| 401 | Missing or wrong bearer token | Check `EXECUTOR_TOKEN` |
| 403 | Caller IP not allowed, key not in allowlist, or `/type`/`/click` disabled | Check config |
| 404 | ERP window not found (not open, or title changed) | Stop and alert |
| 409 | Couldn't focus the ERP, ERP lost focus mid-sequence, window minimized, Caps Lock on, or clipboard busy | Stop, re-check state with a screenshot; never blindly retry input |
| 413 | `/type` text over the max length (default 2000 chars) | Split the text |
| 423 | Paused (via `/pause` or failsafe) | Stop and wait for a human |
| 500 | Windows API failure (input blocked, capture failed) | Stop and alert |
| 503 | Workstation locked or secure desktop showing (also returned by `/health` when not ready) | Stop and alert |
| 504 | `/copy`: clipboard didn't change in time | Nothing was selected, or clipboard redirection is off |

Unknown paths return 404 and wrong methods 405 as plain text (Go's router), not JSON.

## The Window object

Returned by `/health`, `/windows` and `/focus`:

```json
{
  "hwnd": 3212538,
  "title": "KINGS RIVER PACKING, LP - FAMOUS (KRP-AVD-Workspace)",
  "class": "RAIL_WINDOW",
  "pid": 37800,
  "rect": {"x": 0, "y": 0, "w": 1648, "h": 1261},
  "minimized": false
}
```

"Dialogs" are the other visible, non-minimized windows owned by the same process as the main
window. In RemoteApp, every ERP dialog/popup appears as its own local window, so a change in
dialog count or titles is a cheap signal that a popup opened or closed.

## Endpoints

### `GET /health`

Readiness check. Returns 200 when ready, 503 when not. Has no side effects (doesn't focus).

```json
{
  "ok": true,
  "paused": false,
  "input_desktop": true,
  "main": { ...Window },
  "dialogs": 1,
  "foreground_is_erp": false
}
```

`dialogs` here is a **count**. When not ready, `ok` is false and `error` explains why (not found,
minimized, locked, paused). `foreground_is_erp: false` is normal; input calls will focus it.

### `GET /windows`

No side effects.

- Without query: `{"ok": true, "main": Window, "dialogs": [Window, ...]}` (here `dialogs` is an **array**)
- `?all=1`: every visible titled top-level window on the client: `{"ok": true, "windows": [Window, ...]}`

### `POST /focus`

Runs all preconditions and brings the ERP to the foreground. No body.
Response: `{"ok": true, "main": Window, "dialogs": [Window, ...]}`

### `GET /screenshot`

Returns a PNG (`Content-Type: image/png`) of what is visible on screen.

| Query | Values | Default |
|---|---|---|
| `scope` | `window` = main ERP window; `all` = bounding box of main window + dialogs; `screen` = entire virtual desktop | `window` |
| `focus` | `0` = don't bring the ERP to the front first | focus |

While paused, screenshots still work but don't change focus.

Response headers:

| Header | Example | Meaning |
|---|---|---|
| `X-Origin` | `0,0` | Screen coordinates of the image's top-left pixel |
| `X-Main-Rect` | `0,0,1648,1261` | Main window x,y,w,h |
| `X-Dialogs` | `1` | Dialog count at capture time |

Converting an image pixel `(px, py)` to a click: with `scope=window`, send `{"x": px, "y": py}`
(window space) directly. With `scope=all` or `screen`, send
`{"x": originX + px, "y": originY + py, "space": "screen"}`.

### `POST /keys`

Sends key chords in order.

```json
{"keys": ["alt+f", "n", "tab", "tab", "f8"], "delay_ms": 60}
```

- `keys`: 1–200 chords. Required.
- `delay_ms`: pause after each chord, 0–10000. Optional (default 60).

Response: `{"ok": true, "sent": 5}`

**Chord syntax:** modifiers joined with `+`, key last, case-insensitive: `ctrl+shift+s`, `alt+f`,
`f8`, `enter`. A bare modifier is allowed (`alt` alone opens a menu bar).

- Modifiers: `ctrl` (`control`), `shift`, `alt`, `win`
- Letters `a`–`z`, digits `0`–`9`, `f1`–`f24`
- `enter` (`return`), `tab`, `esc` (`escape`), `space`, `backspace` (`bksp`), `delete` (`del`),
  `insert` (`ins`), `home`, `end`, `pgup` (`pageup`), `pgdn` (`pagedown`), `up`, `down`, `left`,
  `right`, `apps`, `comma`, `period`, `minus`, `plus`

For any other characters, use `/type`. If the executor runs with `-allow-keys`, only listed
chords are accepted (403 otherwise).

Keys are sent as hardware scan codes (required for RDP to forward them reliably).

### `POST /type`

Types text into whatever field has focus in the ERP.

```json
{"text": "ACME-1042"}
```

Response: `{"ok": true, "typed": 9}`

- `\n` presses Enter, `\t` presses Tab, `\r` is ignored.
- Max 2000 characters per call by default (413 otherwise).
- Fails with 409 if Caps Lock is on (in the default `keys` typing mode).
- Speed is about 25–40 ms per character.
- For long text, `POST /clipboard` + `/keys ["ctrl+v"]` is faster.

### `POST /click`

```json
{"x": 120, "y": 340, "space": "window", "button": "left", "double": false}
```

| Field | Values | Default |
|---|---|---|
| `x`, `y` | integers | required |
| `space` | `window` (relative to main window's top-left) or `screen` (absolute) | `window` |
| `button` | `left`, `right`, `middle` | `left` |
| `double` | boolean | `false` |

The point must fall inside the main window or one of its dialogs (400 otherwise).
Response: `{"ok": true, "screen": {"x": 120, "y": 340}}`

Prefer keyboard navigation over clicks wherever FAMOUS allows it; clicks depend on layout.

### `POST /copy`

Sends keys (default Ctrl+C), waits for the clipboard to change via RDP clipboard redirection,
and returns its text. This is the most reliable way to read a field value (e.g. an order number).

```json
{"keys": ["ctrl+c"], "timeout_ms": 3000}
```

Both fields are optional (`timeout_ms` is clamped to 100–15000). The body can be empty.
Response: `{"ok": true, "text": "SO-004512"}`

504 means the clipboard didn't change: nothing was selected or copyable. Select the field's
contents first if needed, e.g. `{"keys": ["home", "shift+end", "ctrl+c"]}`.

### `GET /clipboard`

Returns the current clipboard text: `{"ok": true, "text": "..."}`. No input is sent.

### `POST /clipboard`

Sets the clipboard: `{"text": "..."}` → `{"ok": true}`. Follow with `POST /keys {"keys":["ctrl+v"]}`
to paste into the ERP.

### `POST /pause` and `POST /resume`

Emergency stop and restart for all input. No body.
Responses: `{"ok": true, "paused": true}` / `{"ok": true, "paused": false}`.
A `/pause` takes effect mid-sequence (between keystrokes) even while another request is running.

## Guidance for the orchestrator

1. Call `/health` before starting a run; abort if not `ok`.
2. Drive the workflow from code as a fixed sequence of steps. After each step, verify the expected
   state (vision-model check on a `scope=all` screenshot, `X-Dialogs` count, or `/copy` of a field)
   before continuing.
3. Treat any unexpected state or any 409/423/5xx as **stop and alert**, not retry. ERP actions
   like posting an order are not idempotent; a blind retry can create duplicates. Before
   retrying anything, take a screenshot and determine where the ERP actually is.
4. Prefer `/keys` over `/click`, and `/copy` over OCR for reading values.
5. Capture the final confirmation (screenshot + any document number via `/copy`) for the run log.