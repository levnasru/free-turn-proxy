// internal/netctl/bind_windows.go
//go:build windows

package netctl

import (
	"encoding/binary"
	"fmt"
	"net"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

// ipUnicastIF is IP_UNICAST_IF (ws2ipdef.h). golang.org/x/sys/windows does not
// export it by name (verified absent in golang.org/x/sys@v0.47.0, this repo's
// pinned version) — this literal and the byte-swap below match
// golang.zx2c4.com/wireguard/conn/bind_windows.go's bindSocketToInterface4,
// already a transitive dependency of this repo via xray-core, doing the exact
// same thing for the exact same reason.
const ipUnicastIF = 31

// BindToInterface returns a Control func that binds every socket's outbound
// route to iface via IP_UNICAST_IF — the desktop tun-mode equivalent of
// Android's VpnService.protect (see mobile/protect.go): without it, the
// client's own TURN/hub HTTPS traffic gets captured by the very tun routes it
// depends on staying reachable through, and the tunnel deadlocks (routing
// loop).
func BindToInterface(iface string) func(network, address string, c syscall.RawConn) error {
	ifc, err := net.InterfaceByName(iface)
	if err != nil {
		return func(_, _ string, _ syscall.RawConn) error {
			return fmt.Errorf("netctl: resolve interface %q: %w", iface, err)
		}
	}
	// MSDN: for IPv4, IP_UNICAST_IF wants the interface index in network byte
	// order, packed like an IP address with leading zeros — same byte-swap
	// wireguard-go's bindSocketToInterface4 uses for the identical option.
	var idxBytes [4]byte
	binary.BigEndian.PutUint32(idxBytes[:], uint32(ifc.Index))
	idxBE := *(*uint32)(unsafe.Pointer(&idxBytes[0]))

	return func(_, _ string, c syscall.RawConn) error {
		var opErr error
		err := c.Control(func(fd uintptr) {
			opErr = windows.SetsockoptInt(windows.Handle(fd), windows.IPPROTO_IP, ipUnicastIF, int(idxBE))
		})
		if err != nil {
			return err
		}
		return opErr
	}
}
