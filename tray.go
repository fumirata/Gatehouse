package main

import (
	"log/slog"
	"sync"
	"sync/atomic"

	"github.com/energye/systray"
	"github.com/wailsapp/wails/v2/pkg/runtime"

	"github.com/sagan/gatehouse/icons"
	"github.com/sagan/gatehouse/monitor"
)

// trayController owns the notification-area icon and its menu.
//
// Everything in here runs on borrowed threads. onReady is invoked by the
// systray library from its own goroutine, and the Click handlers below run on
// the main OS thread inside the message loop. Neither may block, so any real
// work (registry writes, network) is handed to a goroutine.
type trayController struct {
	app *App

	mu    sync.Mutex
	ready bool
	last  monitor.State

	// visible mirrors the settings window's state, because Wails v2 has no
	// WindowIsVisible to ask. It is kept accurate by every path that shows or
	// hides the window, including the close button via App.onBeforeClose.
	visible atomic.Bool

	statusItem *systray.MenuItem
	lastItem   *systray.MenuItem
	autostart  *systray.MenuItem
}

func newTrayController(app *App) *trayController {
	return &trayController{app: app}
}

// onReady builds the menu. The systray library calls it once, after the tray
// window exists — icon and menu calls made before that point silently fail.
func (t *trayController) onReady() {
	systray.SetIcon(icons.NoCampus)
	systray.SetTooltip("Gatehouse — starting up")

	// Left click toggles the window. No right-click handler is registered, so
	// right click opens the menu by itself.
	systray.SetOnClick(func(systray.IMenu) { t.toggleWindow() })

	t.statusItem = systray.AddMenuItem("Starting up…", "Current state")
	t.statusItem.Disable()

	t.lastItem = systray.AddMenuItem("No attempts yet", "Most recent sign-in attempt")
	t.lastItem.Disable()

	systray.AddSeparator()

	reconnect := systray.AddMenuItem("Reconnect now", "Sign in immediately")
	reconnect.Click(func() { t.reconnectNow() })

	check := systray.AddMenuItem("Check now", "Test connectivity without signing in")
	check.Click(func() { t.checkNow() })

	systray.AddSeparator()

	t.autostart = systray.AddMenuItemCheckbox(
		"Run at login", "Start Gatehouse when you sign in", t.app.GetAutostart())
	// Deliberately off the UI thread: this writes to the registry.
	t.autostart.Click(func() { go t.toggleAutostart() })

	systray.AddSeparator()

	open := systray.AddMenuItem("Settings…", "Open the Gatehouse window")
	open.Click(func() { t.showWindow() })

	quit := systray.AddMenuItem("Quit Gatehouse", "Exit Gatehouse")
	quit.Click(func() { t.app.quitFromTray() })

	t.mu.Lock()
	t.ready = true
	t.mu.Unlock()
}

func (t *trayController) onExit() {}

// apply reflects a monitor snapshot in the tray icon and tooltip.
//
// Called from the monitor's goroutine on every state change. The icon is only
// re-set when the state actually changes: SetIcon writes the icon bytes to a
// temporary file, so it is not something to do on every 15-second poll.
func (t *trayController) apply(s monitor.Snapshot) {
	t.mu.Lock()
	if !t.ready {
		t.mu.Unlock()
		return
	}
	stateChanged := s.State != t.last
	t.last = s.State
	status, lastItem := t.statusItem, t.lastItem
	t.mu.Unlock()

	if stateChanged {
		systray.SetIcon(iconForState(s.State))
	}
	systray.SetTooltip(tooltipFor(s))

	if status != nil {
		status.SetTitle(statusLabel(s))
	}
	if lastItem != nil {
		lastItem.SetTitle(lastAttemptLabel(s))
	}
}

func (t *trayController) setAutostartChecked(enabled bool) {
	t.mu.Lock()
	item := t.autostart
	t.mu.Unlock()

	if item == nil {
		return
	}
	if enabled {
		item.Check()
	} else {
		item.Uncheck()
	}
}

func (t *trayController) toggleAutostart() {
	// Read the registry rather than trusting the checkbox, so the tick follows
	// reality instead of drifting.
	want := !t.app.GetAutostart()
	if err := t.app.SetAutostart(want); err != nil {
		slog.Warn("could not change the run-at-login setting", "err", err)
	}
}

func (t *trayController) reconnectNow() {
	if m := t.app.monitorInstance(); m != nil {
		m.LoginNow()
	}
}

func (t *trayController) checkNow() {
	if m := t.app.monitorInstance(); m != nil {
		m.Wake()
	}
}

// setVisible records the window's state. Called from App.onBeforeClose and from
// every place this package shows or hides the window.
func (t *trayController) setVisible(v bool) { t.visible.Store(v) }

// toggleWindow shows or hides the settings window.
//
// These runtime calls are made straight from the Win32 menu handler, which runs
// on the main thread with the message loop. They are simple ShowWindow calls and
// return promptly; anything slower would have to be moved off this thread.
func (t *trayController) toggleWindow() {
	if t.visible.Load() {
		ctx := t.app.context()
		if ctx == nil {
			return
		}
		runtime.WindowHide(ctx)
		t.setVisible(false)
		return
	}
	t.showWindow()
}

func (t *trayController) showWindow() {
	ctx := t.app.context()
	if ctx == nil {
		return
	}
	runtime.WindowShow(ctx)
	runtime.WindowUnminimise(ctx)
	t.setVisible(true)
}

// quitTray removes the icon from the notification area. It must be called while
// the message loop is still running, or the icon lingers until the user hovers
// over it.
func quitTray() { systray.Quit() }

func iconForState(s monitor.State) []byte {
	switch s {
	case monitor.StateOnline:
		return icons.Online
	case monitor.StateLoggingIn:
		return icons.Connecting
	case monitor.StateLoginFailed, monitor.StateAuthRejected, monitor.StateSuspended:
		return icons.Error
	default:
		// Includes StateConfirming: one check has failed but nothing has been
		// decided yet, so the calm grey icon is the honest one.
		return icons.NoCampus
	}
}

func tooltipFor(s monitor.Snapshot) string {
	base := "Gatehouse — " + statusLabel(s)
	if s.Detail != "" {
		base += "\n" + s.Detail
	}
	return base
}

func statusLabel(s monitor.Snapshot) string {
	switch s.State {
	case monitor.StateOnline:
		return "online"
	case monitor.StateLoggingIn:
		return "signing in…"
	case monitor.StateLoginFailed:
		return "sign-in failed, retrying"
	case monitor.StateAuthRejected:
		return "credentials rejected"
	case monitor.StateSuspended:
		return "paused after repeated failures"
	case monitor.StateConfirming:
		return "checking again…"
	case monitor.StateNotOnCampus:
		return "not on the campus network"
	case monitor.StateNeedsConfig:
		return "not configured yet"
	case monitor.StateMisconfigured:
		return "check the connectivity probe settings"
	default:
		return "starting up"
	}
}

func lastAttemptLabel(s monitor.Snapshot) string {
	if s.LastAttempt == nil {
		return "No attempts yet"
	}
	a := s.LastAttempt
	when := a.At.Format("15:04:05")
	if a.OK {
		return "Last sign-in " + when + ": ok"
	}
	return "Last sign-in " + when + ": " + a.Verdict
}
