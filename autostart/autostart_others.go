//go:build !windows

// Package autostart manages Gatehouse's "run at login" entry.
//
// Gatehouse targets Windows. This file exists so the package still builds
// elsewhere, which keeps `go vet ./...` and the test suite usable on any
// platform.
package autostart

import "errors"

// ErrUnsupported is returned on platforms with no autostart mechanism here.
var ErrUnsupported = errors.New("autostart: only supported on Windows")

// Set is a no-op away from Windows.
func Set(bool) error { return ErrUnsupported }

// Enabled always reports false away from Windows.
func Enabled() (bool, error) { return false, ErrUnsupported }
