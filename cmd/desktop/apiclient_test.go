// cmd/desktop/apiclient_test.go
package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
)

func TestLoginAndFetchConfig(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/login":
			var body struct{ Username, Password string }
			_ = json.NewDecoder(r.Body).Decode(&body)
			if body.Username != "vasya" || body.Password != "pw" {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"token": "tok-abc", "expires_at": 999})
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/config":
			if r.Header.Get("Authorization") != "Bearer tok-abc" {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			_ = json.NewEncoder(w).Encode(DesktopConfig{
				HubURLs: []string{"https://x:8445/turn-creds"}, HubPin: "p", HubToken: "t",
				Peer: "1.2.3.4:56000", ObfProfile: "rtpopus3", ObfKey: "k", Streams: 8,
				SplitMode: "exclude",
			})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	token, err := Login(context.Background(), srv.URL, "vasya", "pw")
	if err != nil {
		t.Fatalf("Login: %v", err)
	}
	if token != "tok-abc" {
		t.Fatalf("expected tok-abc, got %q", token)
	}

	cfg, err := FetchConfig(context.Background(), srv.URL, token)
	if err != nil {
		t.Fatalf("FetchConfig: %v", err)
	}
	if cfg.Streams != 8 || cfg.HubURLs[0] != "https://x:8445/turn-creds" {
		t.Fatalf("unexpected config: %+v", cfg)
	}
}

func TestLoginRejectsBadPassword(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()
	if _, err := Login(context.Background(), srv.URL, "vasya", "wrong"); err == nil {
		t.Fatal("expected error for 401")
	}
}

func TestCacheRoundTrip(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("USERPROFILE", dir) // Windows os.UserHomeDir() reads this

	cfg := &DesktopConfig{HubURLs: []string{"u"}, Streams: 5}
	if err := SaveCache(cfg); err != nil {
		t.Fatalf("SaveCache: %v", err)
	}

	path, err := CachePath()
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("cache file not written: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("expected 0600, got %o", info.Mode().Perm())
	}

	got, err := LoadCache()
	if err != nil {
		t.Fatalf("LoadCache: %v", err)
	}
	if got.Streams != 5 || got.HubURLs[0] != "u" {
		t.Fatalf("round-trip mismatch: %+v", got)
	}
}
