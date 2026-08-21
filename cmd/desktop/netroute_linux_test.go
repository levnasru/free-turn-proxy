//go:build linux

package main

import (
	"errors"
	"testing"
)

func TestDefaultRouteInterfaceParsesIPRouteOutput(t *testing.T) {
	orig := runCommand
	defer func() { runCommand = orig }()
	runCommand = func(name string, args ...string) (string, error) {
		if name != "ip" {
			t.Fatalf("unexpected command %q", name)
		}
		return "default via 192.168.1.1 dev eth0 proto dhcp metric 100 \n", nil
	}

	iface, err := defaultRouteInterface()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if iface != "eth0" {
		t.Fatalf("got %q, want eth0", iface)
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
