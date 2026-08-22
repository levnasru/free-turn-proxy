//go:build linux

package main

import (
	"strings"
	"testing"
)

// A virtual default route shadowing a physical one is the actual "another
// VPN grabbed the default route" signature — must refuse to start tun mode.
func TestCheckEnvironment_RivalVPNShadowingPhysicalRoute(t *testing.T) {
	origRun, origVirtual := runCommand, isVirtualIface
	defer func() { runCommand = origRun; isVirtualIface = origVirtual }()

	runCommand = func(name string, args ...string) (string, error) {
		return "default via 192.168.1.1 dev eth0 proto dhcp metric 100\n" +
			"default via 10.8.0.1 dev tun0 metric 50\n", nil
	}
	isVirtualIface = func(iface string) bool {
		return iface == "tun0" // eth0 physical, tun0 virtual
	}

	err := checkEnvironment(true, nil)
	if err == nil {
		t.Fatal("expected an error when a virtual route shadows a physical one")
	}
	if !strings.Contains(err.Error(), "tun0") {
		t.Fatalf("error = %v, want it to name the rival interface tun0", err)
	}
}

// A topology with *only* virtual default routes (PPPoE's ppp0, a
// bridge-primary setup, a container's veth) is just the user's normal
// uplink, not another VPN — must be allowed through.
func TestCheckEnvironment_OnlyVirtualDefaultRouteIsNotARival(t *testing.T) {
	origRun, origVirtual := runCommand, isVirtualIface
	defer func() { runCommand = origRun; isVirtualIface = origVirtual }()

	runCommand = func(name string, args ...string) (string, error) {
		return "default dev ppp0 metric 50\n", nil // PPPoE: no "via" gateway
	}
	isVirtualIface = func(iface string) bool {
		return true // ppp0 has no /sys/class/net/ppp0/device symlink
	}

	if err := checkEnvironment(true, nil); err != nil {
		t.Fatalf("unexpected error for a PPPoE-only uplink: %v", err)
	}
}
