// internal/netctl/bind_other.go
//go:build !linux && !windows && !darwin

package netctl

import (
	"fmt"
	"syscall"
)

// BindToInterface is unsupported on this platform: there's no portable
// interface-bind socket option here (unlike Linux's SO_BINDTODEVICE, Windows'
// IP_UNICAST_IF, or Darwin's IP_BOUND_IF/IPV6_BOUND_IF). Returns a Control
// func that errors clearly on first use rather than silently no-opping —
// desktop tun-mode's -bind-iface is unreachable on these builds otherwise.
func BindToInterface(iface string) func(network, address string, c syscall.RawConn) error {
	return func(_, _ string, _ syscall.RawConn) error {
		return fmt.Errorf("netctl: -bind-iface (%q) is not supported on this platform", iface)
	}
}
