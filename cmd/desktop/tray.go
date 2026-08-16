// cmd/desktop/tray.go
package main

import (
	"context"

	"github.com/getlantern/systray"
)

// startTray shows a tray icon summarizing statusLabel, wired to the same
// cancel() that Ctrl+C/SIGTERM already stop the tunnel with — "Отключить"/
// "Выход" in the tray are just another way into the exact same shutdown
// path, not a separate one. No-op if trayAvailable() says there's nowhere
// to show it (e.g. a headless Linux SSH session) — the plain
// terminal/Ctrl+C flow keeps working untouched either way.
//
// On Windows this also hides the console window once the tray is up (see
// tray_windows.go) so a connected tunnel doesn't leave a console sitting in
// front of the user; "Развернуть" restores it. Linux/macOS get the
// identical menu through the same systray library (AppIndicator/GTK,
// Cocoa) but keep the terminal visible — a console process here doesn't
// own the terminal emulator's window, so there is nothing to hide.
func startTray(ctx context.Context, cancel context.CancelFunc, statusLabel string) {
	if !trayAvailable() {
		return
	}
	go systray.Run(func() {
		systray.SetTitle("VK-TURN")
		systray.SetTooltip("VK-TURN: " + statusLabel)

		mStatus := systray.AddMenuItem(statusLabel, "")
		mStatus.Disable()
		systray.AddSeparator()
		mRestore := restoreMenuItem() // nil where there's nothing to restore
		mStop := systray.AddMenuItem("Отключить", "Остановить туннель")
		mQuit := systray.AddMenuItem("Выход", "Закрыть VK-TURN")

		hideConsoleOnConnect()

		go func() {
			var restoreCh chan struct{}
			if mRestore != nil {
				restoreCh = mRestore.ClickedCh
			}
			for {
				select {
				case <-restoreCh:
					restoreConsole()
				case <-mStop.ClickedCh:
					cancel()
				case <-mQuit.ClickedCh:
					cancel()
				case <-ctx.Done():
					systray.Quit()
					return
				}
			}
		}()
	}, func() {
		restoreConsole()
	})
}
