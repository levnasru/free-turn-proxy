// cmd/desktop/apiclient.go
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"time"
)

// httpClient replaces http.DefaultClient (which has no timeout) for every
// portal request. Without this, a down/unreachable portal hangs the app
// forever with no feedback — the ctx passed in still applies on top for
// callers that want a tighter deadline.
var httpClient = &http.Client{Timeout: 20 * time.Second}

func Login(ctx context.Context, baseURL, username, password string) (string, error) {
	body, _ := json.Marshal(map[string]string{"username": username, "password": password})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+"/api/v1/login", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := httpClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("login: unexpected status %d", resp.StatusCode)
	}
	var out struct {
		Token string `json:"token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", err
	}
	if out.Token == "" {
		return "", errors.New("login: empty token in response")
	}
	return out.Token, nil
}

func FetchAndroidConfig(ctx context.Context, baseURL, token string) (*DesktopConfig, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/api/v1/config?device=android", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("android config: unexpected status %d", resp.StatusCode)
	}
	var cfg DesktopConfig
	if err := json.NewDecoder(resp.Body).Decode(&cfg); err != nil {
		return nil, err
	}
	return &cfg, nil
}

func FetchConfig(ctx context.Context, baseURL, token string) (*DesktopConfig, error) {
	type aResult struct {
		cfg *DesktopConfig
		err error
	}
	aChan := make(chan aResult, 1)
	go func() {
		ac, ae := FetchAndroidConfig(ctx, baseURL, token)
		aChan <- aResult{cfg: ac, err: ae}
	}()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/api/v1/config", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("config: unexpected status %d", resp.StatusCode)
	}
	var cfg DesktopConfig
	if err := json.NewDecoder(resp.Body).Decode(&cfg); err != nil {
		return nil, err
	}

	// Also fetch WireGuard profile from the portal (device=android provides the WG keypair & peer)
	ar := <-aChan
	if ar.err == nil && ar.cfg != nil {
		cfg.WgConfig = ar.cfg.WgConfig
		cfg.WgPeer = ar.cfg.Peer
	}

	return &cfg, nil
}

func CachePath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".vkturn", "config.json"), nil
}

func SaveCache(cfg *DesktopConfig) error {
	path, err := CachePath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return err
	}
	chownToOriginalUserIfElevated(path)
	return nil
}

func LoadCache() (*DesktopConfig, error) {
	path, err := CachePath()
	if err != nil {
		return nil, err
	}
	return LoadCacheFrom(path)
}

func LoadCacheFrom(path string) (*DesktopConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var cfg DesktopConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, err
	}
	return &cfg, nil
}


type SessionInfo struct {
	BaseURL   string `json:"baseUrl"`
	Token     string `json:"token"`
	ExpiresAt int64  `json:"expiresAt"`
}

func SessionPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".vkturn", "session.json"), nil
}

func SaveSession(sess *SessionInfo) error {
	path, err := SessionPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(sess, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return err
	}
	chownToOriginalUserIfElevated(path)
	return nil
}

func LoadSession() (*SessionInfo, error) {
	path, err := SessionPath()
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var sess SessionInfo
	if err := json.Unmarshal(data, &sess); err != nil {
		return nil, err
	}
	return &sess, nil
}

