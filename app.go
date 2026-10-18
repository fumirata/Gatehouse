package main

import (
	"context"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/wailsapp/wails/v2/pkg/runtime"

	"github.com/sagan/gatehouse/applog"
	"github.com/sagan/gatehouse/autostart"
	"github.com/sagan/gatehouse/config"
	"github.com/sagan/gatehouse/login"
	"github.com/sagan/gatehouse/monitor"
	"github.com/sagan/gatehouse/probe"
)

// App is the object whose methods are exposed to the settings window.
type App struct {
	monitor *monitor.Monitor
	ring    *applog.Ring
	tray    *trayController

	client *http.Client
	dial   probe.DialFunc

	mu     sync.RWMutex
	ctx    context.Context
	cfg    *config.Config
	cancel context.CancelFunc

	quitting atomic.Bool
}

func newApp(cfg *config.Config, ring *applog.Ring) *App {
	// One client serves both probing and logging in: both want redirects
	// disabled, and neither benefits from connection reuse.
	client := login.NewClient()

	a := &App{
		ring:   ring,
		cfg:    cfg,
		client: client,
		dial:   (&net.Dialer{Timeout: 3 * time.Second}).DialContext,
	}
	return a
}

// startup runs once the Wails runtime is live. Wails calls it on its own
// goroutine, so nothing here may assume the main thread.
func (a *App) startup(ctx context.Context) {
	a.mu.Lock()
	a.ctx = ctx
	a.mu.Unlock()

	slog.Info("Gatehouse starting", "config", a.configPath())

	// main starts the window hidden unless there is nothing usable to run with,
	// so the tracked visibility has to start the same way.
	a.tray.setVisible(a.config().Validate() != nil)

	if err := runtime.InitializeNotifications(ctx); err != nil {
		// Cosmetic only: a missing notification path must not stop the app.
		slog.Debug("notifications unavailable", "err", err)
	}

	// Push live state to the settings window.
	onChange := func(s monitor.Snapshot) {
		if c := a.context(); c != nil {
			runtime.EventsEmit(c, "state", s)
		}
		a.tray.apply(s)
	}

	mon := monitor.New(monitor.Options{
		Config:   a.config(),
		Client:   a.client,
		Dial:     a.dial,
		Log:      slog.Default(),
		OnChange: onChange,
		Notify:   a.notify,
	})

	runCtx, cancel := context.WithCancel(context.Background())
	a.mu.Lock()
	a.monitor = mon
	a.cancel = cancel
	a.mu.Unlock()

	a.tray.apply(mon.Snapshot())
	go mon.Run(runCtx)
}

// monitorInstance returns the running monitor, or nil before startup has
// finished. The tray handlers can fire before then.
func (a *App) monitorInstance() *monitor.Monitor {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.monitor
}

// shutdown stops the monitor. Wails calls it after the message loop exits.
func (a *App) shutdown(ctx context.Context) {
	a.mu.RLock()
	cancel := a.cancel
	a.mu.RUnlock()

	if cancel != nil {
		cancel()
	}

	// Give the monitor a moment to unwind, but never hang the exit.
	if mon := a.monitorInstance(); mon != nil {
		select {
		case <-mon.Done():
		case <-time.After(2 * time.Second):
			slog.Warn("the monitor did not stop within 2s; exiting anyway")
		}
	}
	slog.Info("Gatehouse stopped")
}

// notify surfaces a problem to the user, once per failure streak. The monitor
// decides when; this only handles the mechanism.
func (a *App) notify(title, body string) {
	ctx := a.context()
	if ctx == nil {
		return
	}
	if err := runtime.SendNotification(ctx, runtime.NotificationOptions{
		ID:    "gatehouse-attention",
		Title: title,
		Body:  body,
	}); err != nil {
		slog.Debug("could not show a notification", "err", err)
	}
}

func (a *App) context() context.Context {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.ctx
}

func (a *App) config() *config.Config {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.cfg
}

func (a *App) configPath() string {
	p, err := config.Path()
	if err != nil {
		return "(unknown)"
	}
	return p
}

// ---------------------------------------------------------------- bound methods

// GetState returns the monitor's current state, for the window's initial load.
func (a *App) GetState() monitor.Snapshot {
	mon := a.monitorInstance()
	if mon == nil {
		return monitor.Snapshot{State: monitor.StateUnknown, Detail: "starting up"}
	}
	return mon.Snapshot()
}

// GetConfig returns the live configuration. It includes the password in
// plaintext, which is how the user chose to store it and what the settings form
// needs in order to edit it.
func (a *App) GetConfig() *config.Config {
	return a.config()
}

// SaveResult reports what happened to a save, including non-fatal warnings.
type SaveResult struct {
	OK       bool     `json:"ok"`
	Errors   []string `json:"errors"`
	Warnings []string `json:"warnings"`
	Path     string   `json:"path"`
}

// SaveConfig validates and persists the given settings, then applies them to the
// running monitor without a restart.
func (a *App) SaveConfig(next *config.Config) SaveResult {
	res := SaveResult{Path: a.configPath()}

	if next == nil {
		res.Errors = append(res.Errors, "no settings were received")
		return res
	}

	if err := next.Save(); err != nil {
		slog.Error("saving configuration failed", "err", err)
		res.Errors = append(res.Errors, err.Error())
		return res
	}

	// A config that cannot authenticate is still saved — the user may be partway
	// through filling the form in — but it is reported and does not reach the
	// monitor, which stays in its needs-config state.
	if err := next.Validate(); err != nil {
		res.Errors = append(res.Errors, err.Error())
		res.Warnings = append(res.Warnings, next.Warnings()...)
		return res
	}

	res.OK = true
	res.Warnings = append(res.Warnings, next.Warnings()...)

	a.mu.Lock()
	a.cfg = next
	a.mu.Unlock()

	if mon := a.monitorInstance(); mon != nil {
		mon.ApplyConfig(next)
	}
	// Keep the registry entry in step with the setting.
	go a.syncAutostart(next.Autostart)
	return res
}

// TestLogin performs one self-contained check-and-sign-in with the supplied
// settings, without disturbing the running monitor or requiring a save. This is
// the main tool for working out a portal's parameters.
func (a *App) TestLogin(candidate *config.Config) monitor.TestResult {
	cfg := candidate
	if cfg == nil {
		cfg = a.config()
	}

	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()

	slog.Info("manual login test requested")
	return monitor.TestOnce(ctx, a.client, a.dial, cfg)
}

// ReconnectNow forces an immediate attempt, clearing any backoff or pause.
func (a *App) ReconnectNow() {
	if mon := a.monitorInstance(); mon != nil {
		mon.LoginNow()
	}
}

// CheckNow forces an immediate connectivity check without signing in.
func (a *App) CheckNow() {
	if mon := a.monitorInstance(); mon != nil {
		mon.Wake()
	}
}

// GetLog returns the recent log lines, so a failing portal can be diagnosed
// without a console.
func (a *App) GetLog() string {
	if a.ring == nil {
		return ""
	}
	return a.ring.String()
}

// GetAutostart reports whether Gatehouse is set to start when the user signs in.
// The registry is the source of truth; the config value follows it.
func (a *App) GetAutostart() bool {
	enabled, err := autostart.Enabled()
	if err != nil {
		slog.Debug("could not read the autostart entry", "err", err)
		return a.config().Autostart
	}
	return enabled
}

// SetAutostart turns the "run at login" entry on or off.
func (a *App) SetAutostart(enabled bool) error {
	if err := autostart.Set(enabled); err != nil {
		slog.Error("could not update the autostart entry", "err", err)
		return err
	}

	a.mu.Lock()
	a.cfg.Autostart = enabled
	cfg := a.cfg
	a.mu.Unlock()

	if err := cfg.Save(); err != nil {
		slog.Warn("autostart changed but the config could not be saved", "err", err)
	}
	a.tray.setAutostartChecked(enabled)
	return nil
}

func (a *App) syncAutostart(enabled bool) {
	if err := autostart.Set(enabled); err != nil {
		slog.Warn("could not update the autostart entry", "err", err)
		return
	}
	a.tray.setAutostartChecked(enabled)
}

// ShowWindow brings the settings window to the front.
func (a *App) ShowWindow() { a.tray.showWindow() }

// Quit exits Gatehouse.
func (a *App) Quit() { a.quitFromTray() }

// ProbePreset is a known-good connectivity-check endpoint offered in the UI.
type ProbePreset struct {
	Name   string `json:"name"`
	URL    string `json:"url"`
	Status int    `json:"status"`
	Body   string `json:"body"`
	Note   string `json:"note"`
}

// ListProbePresets returns connectivity-check endpoints to choose from.
//
// Having alternatives matters: campus networks do block some of these, and a
// blocked probe URL looks exactly like a permanent outage.
func (a *App) ListProbePresets() []ProbePreset {
	return []ProbePreset{
		{
			Name: "Microsoft NCSI", URL: config.DefaultInternetProbeURL,
			Status: 200, Body: config.DefaultInternetExpectBody,
			Note: "The default. Reachable from most networks, including mainland China.",
		},
		{
			Name: "Xiaomi", URL: "http://connect.rom.miui.com/generate_204",
			Status: 204, Body: "",
			Note: "Small and fast. Good fallback for mainland China.",
		},
		{
			Name: "Huawei", URL: "http://connectivitycheck.platform.hicloud.com/generate_204",
			Status: 204, Body: "",
			Note: "Good fallback for mainland China.",
		},
		{
			Name: "Apple", URL: "http://captive.apple.com/hotspot-detect.html",
			Status: 200, Body: "Success",
			Note: "What iOS uses to detect captive portals.",
		},
		{
			Name: "Google", URL: "http://connectivitycheck.gstatic.com/generate_204",
			Status: 204, Body: "",
			Note: "Blocked on mainland China networks — it will look like a permanent outage there.",
		},
	}
}

// quitFromTray exits the application cleanly.
//
// Order matters and is not arbitrary:
//  1. cancel the monitor, without waiting — this runs on the main thread from a
//     tray menu handler, and blocking there would freeze the UI;
//  2. systray.Quit() posts WM_CLOSE to the tray window, which is what actually
//     removes the icon. It must happen while the message loop is still running;
//  3. runtime.Quit posts WM_QUIT and ends the loop.
//
// Reverse 2 and 3 and a ghost icon is left in the notification area.
func (a *App) quitFromTray() {
	if !a.quitting.CompareAndSwap(false, true) {
		return
	}

	a.mu.RLock()
	cancel, ctx := a.cancel, a.ctx
	a.mu.RUnlock()

	if cancel != nil {
		cancel()
	}
	quitTray()

	if ctx != nil {
		runtime.Quit(ctx)
	}
}

// onSecondInstance runs when the user launches Gatehouse while it is already
// running, and brings the existing window forward instead of starting again.
func (a *App) onSecondInstance() {
	a.tray.showWindow()
}

// onBeforeClose turns the window's close button into "hide to the tray".
//
// The `quitting` gate is what makes this safe. Wails' Frontend.Quit — reached
// from runtime.Quit — consults OnBeforeClose and returns immediately if it says
// prevent, so an ungated handler here would silently disable the tray's Quit
// item. quitFromTray sets the flag before asking Wails to quit, so a real quit
// falls straight through.
//
// Routing the close through here (rather than using HideWindowOnClose) is also
// what lets the tray know whether the window is currently shown: Wails v2 has no
// WindowIsVisible, so its own hidden state would be invisible to us.
func (a *App) onBeforeClose(ctx context.Context) bool {
	if a.quitting.Load() {
		return false
	}
	runtime.WindowHide(ctx)
	a.tray.setVisible(false)
	return true
}

// HideWindow hides the settings window to the tray. Bound for the UI's own
// "hide" affordance.
func (a *App) HideWindow() {
	ctx := a.context()
	if ctx == nil {
		return
	}
	runtime.WindowHide(ctx)
	a.tray.setVisible(false)
}
