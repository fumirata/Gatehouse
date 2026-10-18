//go:build windows

// Package autostart manages Gatehouse's "run at login" entry.
package autostart

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"syscall"

	"golang.org/x/sys/windows/registry"
)

const (
	runKey    = `Software\Microsoft\Windows\CurrentVersion\Run`
	valueName = "Gatehouse"
)

// Set enables or disables launching Gatehouse when the user signs in.
//
// This writes to the registry, so callers on the UI thread must hand it to a
// goroutine — a blocking write on the thread that pumps the message loop would
// freeze the window.
func Set(enabled bool) error {
	k, err := registry.OpenKey(registry.CURRENT_USER, runKey, registry.SET_VALUE|registry.QUERY_VALUE)
	if err != nil {
		return fmt.Errorf("autostart: open HKCU Run key: %w", err)
	}
	defer k.Close()

	if !enabled {
		if err := k.DeleteValue(valueName); err != nil && !errors.Is(err, registry.ErrNotExist) {
			return fmt.Errorf("autostart: remove Run entry: %w", err)
		}
		return nil
	}

	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("autostart: locate executable: %w", err)
	}

	// syscall.EscapeArg applies the CommandLineToArgvW quoting rules that this
	// process itself uses to split os.Args. fmt.Sprintf("%q", exe) would instead
	// escape the backslashes and hand over a literal `C:\\Users\\...` path.
	return k.SetStringValue(valueName, syscall.EscapeArg(exe))
}

// Enabled reports whether the Run entry points at this executable. An entry left
// behind by a moved executable counts as disabled.
func Enabled() (bool, error) {
	k, err := registry.OpenKey(registry.CURRENT_USER, runKey, registry.QUERY_VALUE)
	if err != nil {
		if errors.Is(err, registry.ErrNotExist) {
			return false, nil
		}
		return false, fmt.Errorf("autostart: open HKCU Run key: %w", err)
	}
	defer k.Close()

	stored, _, err := k.GetStringValue(valueName)
	if err != nil {
		if errors.Is(err, registry.ErrNotExist) {
			return false, nil
		}
		return false, fmt.Errorf("autostart: read Run entry: %w", err)
	}

	exe, err := os.Executable()
	if err != nil {
		return false, nil
	}
	return strings.EqualFold(strings.Trim(stored, `"`), exe), nil
}
