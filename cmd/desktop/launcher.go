// cmd/desktop/launcher.go
package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"

	libxray "github.com/xtls/libxray" // module path is lowercase per `go get`; the package itself declares `package libXray`
)

// buildClientArgs mirrors panel.js's genArtifact() for device=linux/windows
// (same flags, same values) — see docs/superpowers/specs/2026-08-14-desktop-client-design.md.
// hubToken travels via env (VKTURN_HUB_TOKEN), never as a flag, so it never
// shows up in `ps`/Task Manager — same reasoning as internal/config/config.go's
// -hub-token flag comment.
func buildClientArgs(cfg *DesktopConfig) (args, env []string) {
	streams := cfg.Streams
	if streams == 0 {
		streams = 10
	}
	args = []string{
		"-provider", "hub",
		"-hub-url", strings.Join(cfg.HubURLs, ","),
		"-hub-pin", cfg.HubPin,
		"-peer", cfg.Peer,
		"-mode", "tcp", "-bond",
		"-obf-profile", cfg.ObfProfile,
		"-obf-key", cfg.ObfKey,
		"-n", strconv.Itoa(streams),
		"-listen", "127.0.0.1:9000",
	}
	env = []string{"VKTURN_HUB_TOKEN=" + cfg.HubToken}
	return args, env
}

// RunClient spawns the `client` binary (expected alongside vkturn-desktop,
// same convention as ftp-client.exe/xray.exe today) and blocks until ctx is
// canceled or the process exits. Canceling ctx sends the process a kill via
// exec.CommandContext's standard behavior — matches how the menu's "exit"
// path stops whichever mode is running.
func RunClient(ctx context.Context, clientBinPath string, cfg *DesktopConfig, stdout, stderr io.Writer) error {
	args, env := buildClientArgs(cfg)
	cmd := exec.CommandContext(ctx, clientBinPath, args...)
	cmd.Env = append(cmd.Env, env...)
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	return cmd.Run()
}

// convertSubscription decodes a 3x-ui-style subscription body (base64 of
// newline-separated share-links, or plain-text share-links if the body
// isn't valid base64) and converts each line to an xray-core JSON config via
// libXray — the same conversion turn-proxy-android's XraySubscriptionFetcher.kt
// already does over JNI; here it's a direct Go call, no bridge needed.
//
// One thing differs from a naive reading of libXray's response shape
// (confirmed against the real go.sum-pinned source at
// github.com/xtls/libxray@v1.260728.0 — go doc's rendered signature plus a
// direct read of the module cache's invoke.go/parse_share.go, not guessed,
// and NOT the newer `main` branch, which has since diverged: main added an
// apiVersion=2 requirement and age-encryption support neither of which exist
// in the version go.sum actually pins):
//   - the convertShareLinksToXrayJson response's "data" field is a JSON
//     *object*, not a pre-encoded JSON string — invokeConvertShareLinksToXrayJson
//     calls share.ConvertShareLinksToXrayJson, which returns (*conf.Config,
//     error) (xray-core's own config struct), and invokeResponse.Data is
//     `any`, so json.Marshal emits it as a nested object under "data". It's
//     captured here as json.RawMessage and used directly as the xray config
//     text, with no extra unmarshal/re-encode step.
//     apiVersion itself needed no change: this pinned version's
//     validateAPIVersion accepts 0 or 1 (no exported version constant exists
//     yet), so the brief's literal `1` is correct as written.
func convertSubscription(body string) ([]string, error) {
	trimmed := strings.TrimSpace(body)
	if trimmed == "" {
		return nil, errors.New("convertSubscription: empty body")
	}
	decoded, err := base64.StdEncoding.DecodeString(trimmed)
	text := trimmed
	if err == nil && strings.Contains(string(decoded), "://") {
		text = string(decoded)
	}

	var configs []string
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		req, _ := json.Marshal(map[string]any{
			"apiVersion": 1,
			"method":     "convertShareLinksToXrayJson",
			"payload":    map[string]string{"text": line},
		})
		respRaw := libxray.Invoke(string(req))
		var resp struct {
			Success bool            `json:"success"`
			Data    json.RawMessage `json:"data"`
		}
		if err := json.Unmarshal([]byte(respRaw), &resp); err != nil || !resp.Success ||
			len(resp.Data) == 0 || string(resp.Data) == "null" {
			continue // skip unparseable lines, same tolerance as the Android fetcher
		}
		configs = append(configs, string(resp.Data))
	}
	return configs, nil
}

// RunXray writes xrayJSON to a temp file and spawns the xray binary
// (expected alongside vkturn-desktop) against it, blocking until ctx is
// canceled or the process exits.
func RunXray(ctx context.Context, xrayBinPath, xrayJSON string, stdout, stderr io.Writer) error {
	tmp, err := os.CreateTemp("", "vkturn-xray-*.json")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.WriteString(xrayJSON); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	cmd := exec.CommandContext(ctx, xrayBinPath, "run", "-c", tmp.Name())
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	return cmd.Run()
}

func clientBinName() string {
	if runtime.GOOS == "windows" {
		return "client.exe"
	}
	return "client"
}

func xrayBinName() string {
	if runtime.GOOS == "windows" {
		return "xray.exe"
	}
	return "xray"
}
