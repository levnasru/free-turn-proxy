package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestCollectBypassRulesIncludesDefaults(t *testing.T) {
	cfg := &DesktopConfig{
		DirectDomains: []string{"custom-site.com"},
		DirectIPs:     []string{"198.51.100.0/24"},
	}
	domains, ips := collectBypassRules(cfg)

	foundDefault := false
	foundCustom := false
	for _, d := range domains {
		if d == "domain:vk.com" {
			foundDefault = true
		}
		if d == "domain:custom-site.com" {
			foundCustom = true
		}
	}
	if !foundDefault {
		t.Errorf("expected default domain:vk.com in bypass rules, got %v", domains)
	}
	if !foundCustom {
		t.Errorf("expected custom domain:custom-site.com in bypass rules, got %v", domains)
	}

	foundIP := false
	for _, ip := range ips {
		if ip == "198.51.100.0/24" {
			foundIP = true
		}
	}
	if !foundIP {
		t.Errorf("expected custom IP in bypass rules, got %v", ips)
	}
}

func TestPublicRoutesWithExclusions(t *testing.T) {
	baseRoutes := publicRoutes()
	// Exclude 8.8.8.0/24
	excludedRoutes := publicRoutesWithExclusions([]string{"8.8.8.0/24"})

	// Excluded routes should have more CIDR blocks because 8.8.8.0/24 punches a hole in the public range
	if len(excludedRoutes) <= len(baseRoutes) {
		t.Errorf("expected more route blocks with exclusion, base: %d, excluded: %d", len(baseRoutes), len(excludedRoutes))
	}
}

func TestAddUserBypassEntryAndLoad(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)

	if err := addUserBypassEntry("example.org"); err != nil {
		t.Fatalf("addUserBypassEntry: %v", err)
	}
	if err := addUserBypassEntry("203.0.113.5"); err != nil {
		t.Fatalf("addUserBypassEntry: %v", err)
	}
	if err := addUserBypassEntry("198.51.100.0/24"); err != nil {
		t.Fatalf("addUserBypassEntry: %v", err)
	}

	domains, ips := loadUserBypassList()
	hasDomain := false
	for _, d := range domains {
		if d == "domain:example.org" {
			hasDomain = true
		}
	}
	if !hasDomain {
		t.Errorf("expected domain:example.org, got %v", domains)
	}

	hasSingleIP := false
	hasSubnet := false
	for _, ip := range ips {
		if ip == "203.0.113.5/32" {
			hasSingleIP = true
		}
		if ip == "198.51.100.0/24" {
			hasSubnet = true
		}
	}
	if !hasSingleIP {
		t.Errorf("expected 203.0.113.5/32, got %v", ips)
	}
	if !hasSubnet {
		t.Errorf("expected 198.51.100.0/24, got %v", ips)
	}

	// Verify file was written to ~/.vkturn/bypass.txt
	expectedPath := filepath.Join(tmpDir, ".vkturn", "bypass.txt")
	if _, err := os.Stat(expectedPath); err != nil {
		t.Fatalf("expected bypass file at %s: %v", expectedPath, err)
	}

	// Test readRawUserBypassEntries
	raw, err := readRawUserBypassEntries()
	if err != nil {
		t.Fatalf("readRawUserBypassEntries: %v", err)
	}
	if len(raw) != 3 {
		t.Fatalf("expected 3 raw entries, got %d: %v", len(raw), raw)
	}

	// Test removeUserBypassEntry
	if err := removeUserBypassEntry(2); err != nil { // remove 203.0.113.5
		t.Fatalf("removeUserBypassEntry: %v", err)
	}
	rawAfter, err := readRawUserBypassEntries()
	if err != nil {
		t.Fatalf("readRawUserBypassEntries after remove: %v", err)
	}
	if len(rawAfter) != 2 || rawAfter[0] != "example.org" || rawAfter[1] != "198.51.100.0/24" {
		t.Fatalf("unexpected raw entries after remove: %v", rawAfter)
	}

	// Test clearUserBypassList
	if err := clearUserBypassList(); err != nil {
		t.Fatalf("clearUserBypassList: %v", err)
	}
	rawCleared, err := readRawUserBypassEntries()
	if err != nil {
		t.Fatalf("readRawUserBypassEntries after clear: %v", err)
	}
	if len(rawCleared) != 0 {
		t.Fatalf("expected 0 entries after clear, got %d: %v", len(rawCleared), rawCleared)
	}
}

