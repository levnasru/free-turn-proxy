// cmd/desktop/elevate_darwin.go
//go:build darwin

package main

import "errors"

// errTunNotSupportedDarwin is returned by relaunchElevated on macOS — vk-turn
// (tun) needs a real elevation-prompt equivalent (AuthorizationExecuteWithPrivileges
// or similar) and real default-route detection (see netroute_darwin.go) that
// this platform doesn't have yet. vk-turn (socks) and xray-подписка don't
// call this at all and are unaffected.
var errTunNotSupportedDarwin = errors.New("vk-turn (tun) пока не поддерживается на macOS")

// isElevated mirrors Linux's euid check — harmless here since
// relaunchElevated below always errors out before this would matter in
// practice (nobody is expected to launch this app as root on macOS), but
// keeping the same shape as elevate_linux.go avoids a third, differently-behaved
// implementation for no reason.
func isElevated() bool {
	return false
}

// relaunchElevated always fails on macOS — see errTunNotSupportedDarwin.
func relaunchElevated(_ []string) error {
	return errTunNotSupportedDarwin
}
