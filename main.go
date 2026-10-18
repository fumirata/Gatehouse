// Command Gatehouse keeps a machine signed in to a university captive portal.
//
// It sits in the Windows notification area, notices when the internet is
// unreachable, confirms the campus portal is actually present, and replays a
// configured login request — then gets out of the way.
package main

import (
	"embed"
	"log/slog"
	"os"

	"github.com/energye/systray"
	"github.com/wailsapp/wails/v2"
	"github.com/wailsapp/wails/v2/pkg/options"
	"github.com/wailsapp/wails/v2/pkg/options/assetserver"

	"github.com/sagan/gatehouse/applog"
	"github.com/sagan/gatehouse/config"
)

//go:embed all:frontend/dist
var assets embed.FS

// singleInstanceID is the unique id Wails uses to find an already-running copy.
const singleInstanceID = "gatehouse-tray-6f2a1c4e"

func main() {
	ring, logErr := applog.Init()
	log := slog.Default()

	cfg, cfgErr := config.Load()
	if cfgErr != nil {
		log.Warn("could not read the settings; starting from defaults", "err", cfgErr)
		cfg = &config.Config{}
	}
	if logErr != nil {
		log.Warn("file logging is unavailable; only the in-memory log will be kept", "err", logErr)
	}
	// Show the window when there is nothing usable to run with, so a first run
	// (or a broken config file) opens the settings instead of starting silently
	// in the tray doing nothing.
	needsSetup := cfg.Validate() != nil

	app := newApp(cfg, ring)
	app.tray = newTrayController(app)

	// ---------------------------------------------------------------------
	// THE ORDER OF THE NEXT FEW LINES IS LOAD-BEARING. Do not move the tray
	// setup into OnStartup, and do not reorder it after wails.Run.
	//
	// systray's package init() calls runtime.LockOSThread(), pinning this
	// goroutine to the main OS thread. RunWithExternalLoop calls registerSystray()
	// synchronously, and that is what creates the tray window and adds the icon —
	// on whichever thread calls it.
	//
	// Wails' message loop is what pumps that window: on Windows it calls
	// GetMessage with hwnd = 0, which receives messages for every window owned by
	// the calling thread. So the tray window has to be created here, on the main
	// goroutine, before wails.Run.
	//
	// Wails invokes OnStartup from a freshly spawned goroutine. Calling this from
	// there would create the tray window on a thread that nobody pumps, and the
	// menu would silently never respond to clicks.
	// ---------------------------------------------------------------------
	startTray, endTray := systray.RunWithExternalLoop(app.tray.onReady, app.tray.onExit)

	// On Windows this only parks a goroutine on an empty message queue, because
	// Wails' loop is the one doing the real pumping. Called anyway so the
	// library's documented contract is honoured.
	startTray()
	defer endTray()

	err := wails.Run(&options.App{
		Title:     "Gatehouse",
		Width:     700,
		Height:    800,
		MinWidth:  620,
		MinHeight: 560,

		StartHidden: needsSetup == false,

		// Closing the window hides it to the tray rather than quitting.
		//
		// This deliberately does NOT use HideWindowOnClose. That option hides the
		// window without telling us, and Wails v2 exposes no way to query whether
		// a window is visible (there is no WindowIsVisible), so the tray's
		// click-to-toggle could never know which way to toggle.
		//
		// Routing the close through OnBeforeClose gives an exact signal, and it is
		// safe only because App.onBeforeClose is gated on the `quitting` flag:
		// Wails' Frontend.Quit — which is what runtime.Quit calls — consults
		// OnBeforeClose first and silently does nothing if it returns true, so an
		// ungated handler would turn the tray's Quit item into a no-op.
		OnBeforeClose: app.onBeforeClose,

		// Matches --bg in the stylesheet, so the window does not flash a different
		// colour before the page paints.
		BackgroundColour: &options.RGBA{R: 244, G: 246, B: 249, A: 1},
		AssetServer:      &assetserver.Options{Assets: assets},

		OnStartup:  app.startup,
		OnShutdown: app.shutdown,

		Bind: []interface{}{app},

		// Launching Gatehouse twice raises the existing window instead of
		// starting a second tray icon.
		SingleInstanceLock: &options.SingleInstanceLock{
			UniqueId: singleInstanceID,
			OnSecondInstanceLaunch: func(options.SecondInstanceData) {
				app.onSecondInstance()
			},
		},
	})
	if err != nil {
		log.Error("Gatehouse could not start", "err", err)
		os.Exit(1)
	}
}
