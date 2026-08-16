//go:build windows

// cmd/desktop/tray_windows.go
package main

import (
	"github.com/getlantern/systray"
	"golang.org/x/sys/windows"
)

var (
	kernel32                = windows.NewLazySystemDLL("kernel32.dll")
	user32                  = windows.NewLazySystemDLL("user32.dll")
	procGetConsoleWindow    = kernel32.NewProc("GetConsoleWindow")
	procShowWindow          = user32.NewProc("ShowWindow")
	procSetForegroundWindow = user32.NewProc("SetForegroundWindow")
)

const (
	swHide = 0
	swShow = 5
)

// trayAvailable is always true on Windows: a console app always has a
// window station to attach a tray icon to (even the rare no-console case
// just makes hideConsoleOnConnect/restoreConsole no-ops below).
func trayAvailable() bool { return true }

func hideConsoleOnConnect() {
	if hwnd, _, _ := procGetConsoleWindow.Call(); hwnd != 0 {
		procShowWindow.Call(hwnd, swHide)
	}
}

func restoreConsole() {
	if hwnd, _, _ := procGetConsoleWindow.Call(); hwnd != 0 {
		procShowWindow.Call(hwnd, swShow)
		procSetForegroundWindow.Call(hwnd)
	}
}

// restoreMenuItem adds the "Развернуть" item that un-hides the console —
// only meaningful where hideConsoleOnConnect actually hid something.
func restoreMenuItem() *systray.MenuItem {
	return systray.AddMenuItem("Развернуть", "Показать консоль")
}
