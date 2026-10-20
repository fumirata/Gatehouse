# Gatehouse

A Windows tray application that keeps you signed in to a university captive
portal. It notices when the internet is unreachable, confirms the campus portal
is actually there, replays your login request, and gets out of the way.

## The rule it follows

```
every check (every few seconds, a ~50ms LAN call):
  ask the portal: is this machine signed in?
    ├─ yes                 ──►  online, do nothing
    ├─ no                  ──►  sign in now, then re-check
    ├─ does not answer     ──►  not on campus, do nothing
    └─ unreadable reply    ──►  do nothing, and say so
```

Gatehouse asks the portal's own status endpoint rather than probing the
internet. That is both faster — a LAN round trip instead of an internet one — and
unambiguous: the portal knows whether it has a session for this client, whereas a
failed internet check only implies that *something* is wrong. Because the answer
is authoritative, an expired session is acted on immediately rather than after a
confirmation delay.

Off campus the portal simply does not answer, so nothing is sent and a hotel,
airport, or cafe portal never sees your campus password. Nothing is keyed off the
Wi-Fi SSID, so Ethernet works exactly the same way.

The one reply that must never be misread is an *unreadable* one — an HTTP error,
an HTML page, an interception. Anything that is not a clean, parsed "you are not
signed in" is treated as "do nothing", so a broken portal cannot trigger a login
on every cycle.

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
| Portal reports a session | Nothing. No request is sent. |
| Portal reports no session | One login request, then a re-check. Typically a few seconds after the session expires. |
| Portal does not answer | Nothing, and the tray icon turns grey — this is not the campus network. |
| Portal's reply is unreadable | Nothing, and the status says so. A broken portal is never taken as "sign in". |
| Login failed | Retries at 5s, 10s, 20s … capped at 5 minutes. |
| Portal rejected the password | Backs off 5 minutes and notifies once. |
| Rejected 3× in a row | Pauses for 30 minutes and tells you. |

How often the portal is asked is configurable (1–60s, default 5s). It affects
only how quickly an expired session is noticed; it does not change how often a
failed sign-in is retried.

Closing the window hides it to the tray; **Quit Gatehouse** in the tray menu
exits. Launching it again while it is running just brings the window forward.

## Layout

```
main.go        wiring: tray first (on the main thread), then Wails
app.go         the object exposed to the settings window
tray.go        notification-area icon and menu
config/        settings file, defaults, validation
probe/         asks the portal whether this machine is signed in
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
