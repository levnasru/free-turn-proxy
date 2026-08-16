//go:build !windows

// cmd/desktop/tray_other.go
package main

import (
	"os"
	"runtime"

	"github.com/getlantern/systray"
)

// A console process on Linux/macOS doesn't own the terminal emulator's
// window, so unlike Windows there's nothing to hide/restore — the tray
// menu (Отключить/Выход) still works identically via the same systray
// library (AppIndicator/GTK on Linux, Cocoa on macOS).
func hideConsoleOnConnect() {}
func restoreConsole()       {}

func restoreMenuItem() *systray.MenuItem { return nil }

// trayAvailable is best-effort on Linux: the AppIndicator/GTK backend needs
// a display, and without DISPLAY/WAYLAND_DISPLAY (a headless SSH session,
// which this project's maintainer does use for admin/testing) attempting
// one risks hanging instead of just failing - skip it there rather than
// degrade a working plain-terminal flow. macOS launched from Terminal.app
// always has a display, so this check only applies to Linux.
func trayAvailable() bool {
	if runtime.GOOS != "linux" {
		return true
	}
	return os.Getenv("DISPLAY") != "" || os.Getenv("WAYLAND_DISPLAY") != ""
}
