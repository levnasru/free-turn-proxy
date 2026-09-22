//go:build windows || (darwin && cgo) || (linux && cgo && appindicator)

// cmd/desktop/tray.go
package main

import (
	"context"
	"os"
	"sync"
	"time"

	"github.com/getlantern/systray"
)

var (
	trayOnce          sync.Once
	trayMu            sync.Mutex
	trayCurrentCancel context.CancelFunc
	mStatusItem       *systray.MenuItem
	mStopItem         *systray.MenuItem
)

// startTray initializes or updates the persistent tray icon.
// getlantern/systray is not re-entrant, so systray.Run is called exactly once.
// Subsequent calls update the status label and wire cancel() to the active mode.
func startTray(ctx context.Context, cancel context.CancelFunc, statusLabel string) {
	if !trayAvailable() {
		return
	}

	trayMu.Lock()
	trayCurrentCancel = cancel
	trayMu.Unlock()

	trayOnce.Do(func() {
		go systray.Run(func() {
			systray.SetTitle("VK-TURN")
			systray.SetTooltip("VK-TURN: " + statusLabel)

			mStatusItem = systray.AddMenuItem(statusLabel, "")
			mStatusItem.Disable()
			systray.AddSeparator()
			mRestore := restoreMenuItem() // nil on non-Windows
			mBypass := systray.AddMenuItem("Сайты мимо туннеля (Whitelist)...", "Редактировать список доменов и IP для прямого доступа")
			mStopItem = systray.AddMenuItem("Отключить", "Остановить туннель")
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
					case <-mBypass.ClickedCh:
						if p, err := bypassFilePath(); err == nil {
							_ = openInSystemEditor(p)
						}
					case <-mStopItem.ClickedCh:
						trayMu.Lock()
						c := trayCurrentCancel
						trayMu.Unlock()
						if c != nil {
							c()
						}
					case <-mQuit.ClickedCh:
						trayMu.Lock()
						c := trayCurrentCancel
						trayMu.Unlock()
						if c != nil {
							c()
						}
						restoreConsole()
						systray.Quit()
						go func() {
							time.Sleep(3 * time.Second)
							os.Exit(0)
						}()
						return
					}
				}
			}()
		}, func() {
			restoreConsole()
		})
	})

	// If tray is already running, update status and hide console if needed
	trayMu.Lock()
	if mStatusItem != nil {
		mStatusItem.SetTitle(statusLabel)
		systray.SetTooltip("VK-TURN: " + statusLabel)
	}
	if mStopItem != nil {
		mStopItem.Enable()
	}
	trayMu.Unlock()
	hideConsoleOnConnect()

	// Monitor context cancellation to update tray state without killing systray
	go func() {
		<-ctx.Done()
		trayMu.Lock()
		if mStatusItem != nil {
			mStatusItem.SetTitle("Отключено")
			systray.SetTooltip("VK-TURN: Отключено")
		}
		if mStopItem != nil {
			mStopItem.Disable()
		}
		trayMu.Unlock()
		restoreConsole()
	}()
}
