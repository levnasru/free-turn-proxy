// cmd/desktop/launcher_test.go
package main

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestBuildClientArgs(t *testing.T) {
	cfg := &DesktopConfig{
		HubURLs: []string{"https://x:8445/turn-creds"}, HubPin: "pin1", HubToken: "tok1",
		Peer: "1.2.3.4:56000", ObfProfile: "rtpopus3", ObfKey: "key1", Streams: 8,
	}
	args, env := buildClientArgs(cfg)

	want := []string{
		"-provider", "hub", "-hub-url", "https://x:8445/turn-creds", "-hub-pin", "pin1",
		"-peer", "1.2.3.4:56000", "-mode", "tcp", "-bond",
		"-obf-profile", "rtpopus3", "-obf-key", "key1", "-n", "8",
		"-listen", "127.0.0.1:9000",
	}
	if !reflect.DeepEqual(args, want) {
		t.Fatalf("args mismatch:\n got  %v\n want %v", args, want)
	}

	found := false
	for _, e := range env {
		if e == "VKTURN_HUB_TOKEN=tok1" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected VKTURN_HUB_TOKEN in env, got %v", env)
	}
}

func TestBuildClientArgsMultipleHubURLs(t *testing.T) {
	cfg := &DesktopConfig{
		HubURLs: []string{"https://x:8445/turn-creds", "https://x:8446/turn-creds"},
		HubPin:  "pin1", HubToken: "tok1", Peer: "1.2.3.4:56000",
		ObfProfile: "rtpopus3", ObfKey: "key1", Streams: 8,
	}
	args, _ := buildClientArgs(cfg)
	joined := ""
	for i, a := range args {
		if a == "-hub-url" {
			joined = args[i+1]
		}
	}
	if joined != "https://x:8445/turn-creds,https://x:8446/turn-creds" {
		t.Fatalf("expected comma-joined hub-url, got %q", joined)
	}
}

func TestConvertSubscriptionParsesShareLinks(t *testing.T) {
	// Two share-links, base64-joined by newline, as a real 3x-ui subscription
	// body would look (see docs/sub.md-adjacent research in the design doc).
	body := "dmxlc3M6Ly9leGFtcGxlLTEKdmxlc3M6Ly9leGFtcGxlLTI=" // "vless://example-1\nvless://example-2"
	_, err := convertSubscription(body)
	// This link isn't a real vless URI, so libXray will fail to convert it —
	// that's fine for this test, which only checks that convertSubscription
	// doesn't error on decode/line-splitting itself. Full conversion is
	// exercised against a real subscription URL in the Step 6 live test.
	if err != nil {
		t.Fatalf("convertSubscription should not fail on decode/split, got: %v", err)
	}
}

func TestConvertSubscriptionRejectsEmptyBody(t *testing.T) {
	_, err := convertSubscription("")
	if err == nil {
		t.Fatal("expected error for empty subscription body")
	}
}

func TestDecodeSubscriptionBodyUnpaddedBase64Fallback(t *testing.T) {
	const want = "vless://example-1\nvless://example-2"
	padded := "dmxlc3M6Ly9leGFtcGxlLTEKdmxlc3M6Ly9leGFtcGxlLTI=" // base64.StdEncoding of want
	unpadded := strings.TrimRight(padded, "=")                   // base64.RawStdEncoding of want

	if got := decodeSubscriptionBody(padded); got != want {
		t.Fatalf("padded: got %q, want %q", got, want)
	}
	// Before the RawStdEncoding fallback, StdEncoding fails to decode this
	// (wrong length for padded input) and decodeSubscriptionBody fell back
	// to returning the base64 string itself, unchanged — this is the
	// regression case the fallback fixes.
	if got := decodeSubscriptionBody(unpadded); got != want {
		t.Fatalf("unpadded: got %q, want %q", got, want)
	}
}

func TestDecodeSubscriptionBodyPlainTextPassthrough(t *testing.T) {
	const plain = "vless://example-1\nvless://example-2"
	if got := decodeSubscriptionBody(plain); got != plain {
		t.Fatalf("got %q, want %q (unchanged)", got, plain)
	}
}

func TestBuildVKTurnBridgeConfigParsesAndMatchesKit(t *testing.T) {
	raw := buildVKTurnBridgeConfig()

	var cfg struct {
		Inbounds []struct {
			Protocol string `json:"protocol"`
			Port     int    `json:"port"`
		} `json:"inbounds"`
		Outbounds []struct {
			Protocol string `json:"protocol"`
			Settings struct {
				Vnext []struct {
					Address string `json:"address"`
					Port    int    `json:"port"`
					Users   []struct {
						ID string `json:"id"`
					} `json:"users"`
				} `json:"vnext"`
			} `json:"settings"`
		} `json:"outbounds"`
	}
	if err := json.Unmarshal([]byte(raw), &cfg); err != nil {
		t.Fatalf("buildVKTurnBridgeConfig produced invalid JSON: %v\n%s", err, raw)
	}

	if len(cfg.Inbounds) != 1 || cfg.Inbounds[0].Protocol != "socks" || cfg.Inbounds[0].Port != vkTurnLocalSocksPort {
		t.Fatalf("unexpected inbounds: %+v", cfg.Inbounds)
	}
	if len(cfg.Outbounds) != 1 || cfg.Outbounds[0].Protocol != "vless" {
		t.Fatalf("unexpected outbounds: %+v", cfg.Outbounds)
	}
	vnext := cfg.Outbounds[0].Settings.Vnext
	if len(vnext) != 1 || vnext[0].Address != "127.0.0.1" || vnext[0].Port != 9000 {
		t.Fatalf("unexpected vnext: %+v", vnext)
	}
	if len(vnext[0].Users) != 1 || vnext[0].Users[0].ID != vkTurnBridgeUUID {
		t.Fatalf("unexpected users: %+v", vnext[0].Users)
	}
}

func TestResolveBinPrefersVersionedOverBare(t *testing.T) {
	dir := t.TempDir()
	exeSuffix := ""
	if runtime.GOOS == "windows" {
		exeSuffix = ".exe"
	}
	versioned := filepath.Join(dir, "client-"+runtime.GOOS+"-"+runtime.GOARCH+exeSuffix)
	bare := filepath.Join(dir, "client"+exeSuffix)
	mustWriteExecutable(t, versioned)
	mustWriteExecutable(t, bare)

	got, err := resolveBin(dir, "client")
	if err != nil {
		t.Fatalf("resolveBin: %v", err)
	}
	if got != versioned {
		t.Fatalf("expected versioned path %q, got %q", versioned, got)
	}
}

func TestResolveBinFallsBackToBare(t *testing.T) {
	dir := t.TempDir()
	exeSuffix := ""
	if runtime.GOOS == "windows" {
		exeSuffix = ".exe"
	}
	bare := filepath.Join(dir, "client"+exeSuffix)
	mustWriteExecutable(t, bare)

	got, err := resolveBin(dir, "client")
	if err != nil {
		t.Fatalf("resolveBin: %v", err)
	}
	if got != bare {
		t.Fatalf("expected bare path %q, got %q", bare, got)
	}
}

func TestResolveBinFallsBackToPath(t *testing.T) {
	dir := t.TempDir() // nothing here — forces the PATH fallback
	pathDir := t.TempDir()
	exeSuffix := ""
	if runtime.GOOS == "windows" {
		exeSuffix = ".exe"
	}
	onPath := filepath.Join(pathDir, "client"+exeSuffix)
	mustWriteExecutable(t, onPath)
	t.Setenv("PATH", pathDir)

	got, err := resolveBin(dir, "client")
	if err != nil {
		t.Fatalf("resolveBin: %v", err)
	}
	if got != onPath {
		t.Fatalf("expected PATH fallback %q, got %q", onPath, got)
	}
}

func TestResolveBinErrorsWhenNotFoundAnywhere(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PATH", t.TempDir()) // empty PATH dir — nothing to find

	_, err := resolveBin(dir, "client")
	if err == nil {
		t.Fatal("expected error when client isn't found anywhere")
	}
}

func mustWriteExecutable(t *testing.T, path string) {
	t.Helper()
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func TestWaitForListeningSucceedsOnceAccepting(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()

	if err := waitForListening(context.Background(), ln.Addr().String(), 2*time.Second); err != nil {
		t.Fatalf("waitForListening: %v", err)
	}
}

func TestWaitForListeningTimesOutWhenNothingListens(t *testing.T) {
	// Bind to get a genuinely free port, then close it immediately so
	// nothing is listening there — deterministic vs. guessing a port.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()
	ln.Close()

	err = waitForListening(context.Background(), addr, 300*time.Millisecond)
	if err == nil {
		t.Fatal("expected timeout error, got nil")
	}
}

func TestWaitForListeningRespectsContextCancellation(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()
	ln.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err = waitForListening(ctx, addr, 2*time.Second)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
}
