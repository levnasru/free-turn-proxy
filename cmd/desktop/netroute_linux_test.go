//go:build linux

package main

import (
	"errors"
	"testing"
)

func TestDefaultRouteInterfaceParsesIPRouteOutput(t *testing.T) {
	origRun, origVirtual := runCommand, isVirtualIface
	defer func() { runCommand = origRun; isVirtualIface = origVirtual }()

	// Virtual interface listed first, physical second — proves the
	// physical-preference branch actually wins instead of the fallback
	// (first-line) branch silently doing the work instead.
	runCommand = func(name string, args ...string) (string, error) {
		if name != "ip" {
			t.Fatalf("unexpected command %q", name)
		}
		return "default via 10.8.0.1 dev tun0 metric 50\ndefault via 192.168.1.1 dev eth0 proto dhcp metric 100 \n", nil
	}
	isVirtualIface = func(iface string) bool {
		return iface != "eth0" // only eth0 is physical
	}

	iface, err := defaultRouteInterface()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if iface != "eth0" {
		t.Fatalf("got %q, want eth0 (physical should win over virtual tun0)", iface)
	}
}

func TestDefaultRouteInterfaceNoDefaultRoute(t *testing.T) {
	orig := runCommand
	defer func() { runCommand = orig }()
	runCommand = func(name string, args ...string) (string, error) {
		return "", nil // empty output: no default route configured
	}

	_, err := defaultRouteInterface()
	if !errors.Is(err, errNoDefaultRoute) {
		t.Fatalf("got err=%v, want errNoDefaultRoute", err)
	}
}

// A route line that exists but doesn't match the expected "dev <iface>"
// pattern means "couldn't parse what's there", not "no route configured" —
// these are different failure modes and must not collapse into the same
// error.
func TestDefaultRouteInterfaceUnparseableLineIsNotNoDefaultRoute(t *testing.T) {
	orig := runCommand
	defer func() { runCommand = orig }()
	runCommand = func(name string, args ...string) (string, error) {
		return "not a route line at all\n", nil
	}

	_, err := defaultRouteInterface()
	if err == nil {
		t.Fatal("expected an error for unparseable route output")
	}
	if errors.Is(err, errNoDefaultRoute) {
		t.Fatal("unparseable route output must not report errNoDefaultRoute — a route exists, it just didn't parse")
	}
	if !errors.Is(err, errRouteUnparseable) {
		t.Fatalf("got err=%v, want errRouteUnparseable", err)
	}
}
