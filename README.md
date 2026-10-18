# Gatehouse

A Windows tray application that keeps you signed in to a university captive
portal. It notices when the internet is unreachable, confirms the campus portal
is actually there, replays your login request, and gets out of the way.

## The rule it follows

```
every check:
  is the internet reachable?  ── yes ──►  online, do nothing
           │ no
           ▼
  does the portal host answer? ── no ──►  not on campus, do nothing
           │ yes
           ▼
        sign in, then re-check
```

The second question is the important one. Because credentials are only sent when
the **configured** portal host answers, a hotel, airport, or cafe portal never
sees your campus password. Nothing is keyed off the Wi-Fi SSID, so Ethernet works
exactly the same way.

## Install

Build it (needs Go 1.25+, Node, and the WebView2 runtime that ships with
Windows 11):

```bash
go install github.com/wailsapp/wails/v2/cmd/wails@v2.14.0
```

```bash
wails build -platform windows/amd64
```

The result is `build/bin/Gatehouse.exe`, a single self-contained file with no
console window. Copy it anywhere and run it.

To start with Windows, tick **Run at login** in the tray menu or in the settings
window; this writes a `HKCU\Software\Microsoft\Windows\CurrentVersion\Run` entry.

## Configure

There are only three things to set: username, password, and the service outlet
(学生电信 / 学生联通 / 学生移动). Enter the bare account number — the outlet
suffix (`@aust` / `@unicom` / `@cmcc`) is appended automatically, exactly as the
portal's own login page does.

Both the portal address and the login request are compiled in — it is the Dr.COM
handshake this campus uses, so there is nothing to capture or paste:

```
GET http://10.255.0.19/drcom/login
  ?callback=dr1003
  &DDDDD=<username>
  &upass=<password>
  &0MKKey=123456&R1=0&R3=0&R6=0&para=00&v6ip=&v=<cache-buster>
```

The portal replies with JSONP — `dr1003({...})` — and Gatehouse reads
`"login_result":1` as success and `"result":0` as a rejection. The `v=` value is
regenerated on every attempt so no cache can answer with a stale response.

Press **Test login** to try it immediately; it works the settings currently on
screen, so you do not have to save first. If it fails, **Recent attempts** shows
the portal's own reply.

Settings live in `%APPDATA%\Gatehouse\config.json` and the log in
`%APPDATA%\Gatehouse\gatehouse.log`. **The password is stored in plaintext** —
that is a deliberate choice, and the reason the app is strict about never sending
it anywhere except the configured portal.

## Behaviour

| Situation | What happens |
|---|---|
| Internet works | Nothing. No request is sent. |
| Offline, portal not reachable | Nothing, and the tray icon turns grey. |
| Offline, portal reachable | One login request, then a re-check. |
| Login failed | Retries at 5s, 10s, 20s … capped at 5 minutes. |
| Portal rejected the password | Backs off 5 minutes and notifies once. |
| Rejected 3× in a row | Pauses for 30 minutes and tells you. |

Closing the window hides it to the tray; **Quit Gatehouse** in the tray menu
exits. Launching it again while it is running just brings the window forward.

## Layout

```
main.go        wiring: tray first (on the main thread), then Wails
app.go         the object exposed to the settings window
tray.go        notification-area icon and menu
config/        settings file, defaults, validation
probe/         "is the internet up?" and "is the portal there?"
login/         the Dr.COM request, credential escaping, response verdicts
monitor/       the state machine: when to act, backoff, suspension
autostart/     the HKCU Run entry
applog/        slog to file + an in-memory ring for the UI
icons/         embedded tray icons (regenerate with tools/genicons.py)
frontend/      the settings window (vanilla JS + Vite)
```

## Notes on the stack

Wails v2.14.0 is used rather than v3 because v3 is still in beta. v2 has no tray
API of its own, so the icon comes from `energye/systray`, which is pure Go on
Windows — no C compiler is needed. Two details in this arrangement are easy to
break and are commented at their call sites:

- `systray.RunWithExternalLoop` **must** be called from `main`, before
  `wails.Run`. It creates the tray window on the calling thread, and Wails'
  message loop is what pumps it. Moving it into `OnStartup` would create the
  window on a thread nobody pumps, and the menu would silently stop responding.
- `OnBeforeClose` **must** be gated on the `quitting` flag. Wails' own `Quit`
  consults it and returns early when it says "prevent", so an ungated handler
  would make the tray's Quit item do nothing.

## Tests

```bash
go test ./...
```

The tests need no network. A fake portal stands in for both the internet and the
campus network, and the clock is injected, so backoff and suspension are verified
without waiting. They cover the guarantees that matter most: nothing is sent when
online, nothing is sent when the portal is absent, passwords containing `&` or
`=` survive encoding, and credentials are never forwarded across a redirect.
