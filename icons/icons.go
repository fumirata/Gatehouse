// Package icons holds the tray icon assets.
//
// Windows requires real .ico payloads here, not PNGs: energye/systray writes the
// bytes to a temporary file and hands them to LoadImage with LR_LOADFROMFILE,
// which only understands the ICO container. Each file carries 16/20/24/32/48/64/
// 128/256px variants so the icon stays crisp at any taskbar scaling.
//
// Regenerate with: python tools/genicons.py
package icons

import _ "embed"

// Online is shown when the internet is reachable.
//
//go:embed online.ico
var Online []byte

// Connecting is shown while a login request is in flight.
//
//go:embed connecting.ico
var Connecting []byte

// Error is shown when a login was rejected or repeatedly failed.
//
//go:embed error.ico
var Error []byte

// NoCampus is shown when the internet is down but the portal is not there
// either — the app is deliberately doing nothing.
//
//go:embed nocampus.ico
var NoCampus []byte
