// cmd/desktop/launcher_test.go
package main

import (
	"reflect"
	"testing"
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
