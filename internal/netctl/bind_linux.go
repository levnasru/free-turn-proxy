// internal/netctl/bind_linux.go
//go:build linux

package netctl

import (
	"syscall"

	"golang.org/x/sys/unix"
)

// BindToInterface returns a Control func that binds every socket to iface via
// SO_BINDTODEVICE — the desktop tun-mode equivalent of Android's
// VpnService.protect (see mobile/protect.go): without it, the client's own
// TURN/hub HTTPS traffic gets captured by the very tun routes it depends on
// staying reachable through, and the tunnel deadlocks (routing loop).
func BindToInterface(iface string) func(network, address string, c syscall.RawConn) error {
	return func(_, _ string, c syscall.RawConn) error {
		var opErr error
		err := c.Control(func(fd uintptr) {
			opErr = unix.BindToDevice(int(fd), iface)
		})
		if err != nil {
			return err
		}
		return opErr
	}
}
