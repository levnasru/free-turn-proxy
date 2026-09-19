//go:build !windows && !(darwin && cgo) && !(linux && cgo && appindicator)

// cmd/desktop/tray_stub.go
package main

import "context"

func startTray(ctx context.Context, cancel context.CancelFunc, statusLabel string) {}
func hideConsoleOnConnect()                                                         {}
func restoreConsole()                                                               {}
func trayAvailable() bool                                                           { return false }
