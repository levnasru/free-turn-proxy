// cmd/desktop/wireguard_test.go
package main

import (
	"strings"
	"testing"
)

func TestParseWGConfigValid(t *testing.T) {
	sample := `[Interface]
PrivateKey = iJlfuhNdG4Ul/5TZ9vEHr7HKbLDYGlKt36D/7VRRw2Q=
Address = 10.13.13.14/32
DNS = 1.1.1.1
MTU = 1050

[Peer]
PublicKey = aVt1fPF9nwraL8IuhR1VXleGbrq271uwkCs7GuSL7j0=
AllowedIPs = 0.0.0.0/0
Endpoint = 127.0.0.1:9000
PersistentKeepalive = 25
`
	parsed, err := parseWGConfig(sample)
	if err != nil {
		t.Fatalf("parseWGConfig failed: %v", err)
	}

	if parsed.PrivateKey != "iJlfuhNdG4Ul/5TZ9vEHr7HKbLDYGlKt36D/7VRRw2Q=" {
		t.Errorf("wrong PrivateKey: %s", parsed.PrivateKey)
	}
	if parsed.Address != "10.13.13.14/32" {
		t.Errorf("wrong Address: %s", parsed.Address)
	}
	if parsed.PublicKey != "aVt1fPF9nwraL8IuhR1VXleGbrq271uwkCs7GuSL7j0=" {
		t.Errorf("wrong PublicKey: %s", parsed.PublicKey)
	}
	if parsed.Endpoint != "127.0.0.1:9000" {
		t.Errorf("wrong Endpoint: %s", parsed.Endpoint)
	}
	if parsed.MTU != 1050 {
		t.Errorf("wrong MTU: %d", parsed.MTU)
	}
	if parsed.Keepalive != 25 {
		t.Errorf("wrong Keepalive: %d", parsed.Keepalive)
	}
}

func TestParseWGConfigMissingKeys(t *testing.T) {
	_, err := parseWGConfig("[Interface]\nAddress = 10.13.13.14/32\n")
	if err == nil {
		t.Fatal("expected error on missing PrivateKey")
	}
}

func TestBuildNativeWGConf(t *testing.T) {
	parsed := &WGConfigParsed{
		PrivateKey: "privkey==",
		Address:    "10.13.13.14/32",
		PublicKey:  "pubkey==",
		Endpoint:   "127.0.0.1:9000",
		MTU:        1050,
		Keepalive:  25,
	}

	conf := buildNativeWGConf(parsed, []string{"0.0.0.0/1", "128.0.0.0/1"}, true)
	if !strings.Contains(conf, "PrivateKey = privkey==") {
		t.Errorf("missing PrivateKey in conf: %s", conf)
	}
	if !strings.Contains(conf, "Address = 10.13.13.14/32") {
		t.Errorf("missing Address in conf: %s", conf)
	}
	if !strings.Contains(conf, "PublicKey = pubkey==") {
		t.Errorf("missing PublicKey in conf: %s", conf)
	}
	if !strings.Contains(conf, "Endpoint = 127.0.0.1:9000") {
		t.Errorf("missing Endpoint in conf: %s", conf)
	}
	if !strings.Contains(conf, "AllowedIPs = 0.0.0.0/1, 128.0.0.0/1") {
		t.Errorf("missing AllowedIPs in conf: %s", conf)
	}
	if !strings.Contains(conf, "DNS = 1.1.1.1") {
		t.Errorf("missing DNS in conf: %s", conf)
	}

	// Without DNS
	confNoDNS := buildNativeWGConf(parsed, []string{"0.0.0.0/0"}, false)
	if strings.Contains(confNoDNS, "DNS =") {
		t.Errorf("expected no DNS in conf: %s", confNoDNS)
	}
}
