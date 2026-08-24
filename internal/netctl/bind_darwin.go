// internal/netctl/bind_darwin.go
//go:build darwin

package netctl

import (
	"fmt"
	"net"
	"syscall"

	"golang.org/x/sys/unix"
)

// BindToInterface returns a Control func that binds every socket's outbound
// route to iface via IP_BOUND_IF/IPV6_BOUND_IF (BSD's equivalent of Linux's
// SO_BINDTODEVICE) — the desktop tun-mode equivalent of Android's
// VpnService.protect (see mobile/protect.go): without it, the client's own
// TURN/hub HTTPS traffic gets captured by the very tun routes it depends on
// staying reachable through, and the tunnel deadlocks (routing loop).
func BindToInterface(iface string) func(network, address string, c syscall.RawConn) error {
	ifc, err := net.InterfaceByName(iface)
	if err != nil {
		return func(_, _ string, _ syscall.RawConn) error {
			return fmt.Errorf("netctl: resolve interface %q: %w", iface, err)
		}
	}
	idx := ifc.Index

	return func(_, _ string, c syscall.RawConn) error {
		var opErr error
		err := c.Control(func(fd uintptr) {
			// IP_BOUND_IF/IPV6_BOUND_IF are family-specific — ask the socket
			// its own family (Getsockname) instead of guessing from network/address.
			sa, saErr := unix.Getsockname(int(fd))
			if saErr != nil {
				opErr = saErr
				return
			}
			if _, isV6 := sa.(*unix.SockaddrInet6); isV6 {
				opErr = unix.SetsockoptInt(int(fd), unix.IPPROTO_IPV6, unix.IPV6_BOUND_IF, idx)
			} else {
				opErr = unix.SetsockoptInt(int(fd), unix.IPPROTO_IP, unix.IP_BOUND_IF, idx)
			}
		})
		if err != nil {
			return err
		}
		return opErr
	}
}
