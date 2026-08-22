// cmd/desktop/launcher.go
package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	libxray "github.com/xtls/libxray" // module path is lowercase per `go get`; the package itself declares `package libXray`
	"golang.org/x/net/proxy"
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
	if debugMode {
		args = append(args, "-debug")
	}
	env = []string{"VKTURN_HUB_TOKEN=" + cfg.HubToken}
	return args, env
}

// RunClient spawns the `client` binary (expected alongside vkturn-desktop,
// same convention as ftp-client.exe/xray.exe today) and blocks until ctx is
// canceled or the process exits. Canceling ctx sends the process a kill via
// exec.CommandContext's standard behavior — matches how the menu's "exit"
// path stops whichever mode is running.
func RunClient(ctx context.Context, clientBinPath string, cfg *DesktopConfig, stdout, stderr io.Writer, extraArgs ...string) error {
	args, env := buildClientArgs(cfg)
	args = append(args, extraArgs...)
	cmd := exec.CommandContext(ctx, clientBinPath, args...)
	// cmd.Env starts nil; appending onto nil (instead of os.Environ()) would
	// replace the child's entire environment with just VKTURN_HUB_TOKEN,
	// dropping PATH/HOME/SystemRoot/etc. Start from the parent's env instead.
	cmd.Env = append(os.Environ(), env...)
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
//
// decodeSubscriptionBody decodes a 3x-ui-style subscription body: base64
// (padded or unpadded — some 3x-ui deployments omit padding) of
// newline-separated share-links, falling back to treating trimmed as
// plain-text share-links directly if neither decodes to something
// containing "://".
func decodeSubscriptionBody(trimmed string) string {
	decoded, err := base64.StdEncoding.DecodeString(trimmed)
	if err != nil {
		decoded, err = base64.RawStdEncoding.DecodeString(trimmed)
	}
	if err == nil && strings.Contains(string(decoded), "://") {
		return string(decoded)
	}
	return trimmed
}

func convertSubscription(body string) ([]string, error) {
	trimmed := strings.TrimSpace(body)
	if trimmed == "" {
		return nil, errors.New("convertSubscription: empty body")
	}
	text := decodeSubscriptionBody(trimmed)

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

// vkTurnLocalSocksPort is the local SOCKS5 inbound the xray bridge listens
// on for vk-turn mode — same port the manual onboarding kits' client.json
// uses (see buildVKTurnBridgeConfig).
const vkTurnLocalSocksPort = 1085

// vkTurnBridgeUUID is the VLESS user id the local xray bridge authenticates
// with against cmd/client's raw TCP listener on 127.0.0.1:9000. It's the
// exact UUID the manual onboarding kits (win-kit2, vkturn-linux-kit) already
// use in production — validated server-side by the family's existing VPS
// Xray backend, entirely outside this codebase. Not a secret in the usual
// sense (it authenticates a loopback-only hop between two local processes,
// both already trusted), but still not something to change casually since
// the server side has to match.
const vkTurnBridgeUUID = "5897fcf0-6a80-4761-9917-7091e3618981"

// vkTurnIPCheckURL is the plain outbound IP-echo endpoint used as a
// positive control once the local SOCKS bridge claims to be up — the same
// endpoint win-kit2's Connect.bat already uses for this exact purpose.
const vkTurnIPCheckURL = "https://api.ipify.org"

// buildVKTurnBridgeConfig returns the xray JSON config for the local
// SOCKS-inbound -> VLESS-outbound bridge that makes cmd/client's raw TCP
// listener on 127.0.0.1:9000 usable as an actual proxy. Mirrors the manual
// onboarding kits' client.json exactly (same port, same shared UUID — that
// UUID is validated server-side by the family's existing VPS Xray backend,
// unrelated to and unchanged by this codebase).
func buildVKTurnBridgeConfig() string {
	logLevel := "warning"
	if debugMode {
		logLevel = "debug"
	}
	return fmt.Sprintf(`{
  "log": { "loglevel": %q },
  "inbounds": [
    {
      "protocol": "socks",
      "listen": "127.0.0.1",
      "port": %d,
      "settings": { "udp": true },
      "sniffing": { "enabled": true, "destOverride": ["http", "tls"] }
    }
  ],
  "outbounds": [
    {
      "protocol": "vless",
      "settings": {
        "vnext": [
          {
            "address": "127.0.0.1",
            "port": 9000,
            "users": [
              { "id": %q, "encryption": "none" }
            ]
          }
        ]
      },
      "streamSettings": { "network": "tcp", "security": "none" }
    }
  ]
}`, logLevel, vkTurnLocalSocksPort, vkTurnBridgeUUID)
}

// vkTurnTunInterfaceName is the TUN adapter name xray creates for tun mode —
// arbitrary on Windows/Linux (unlike macOS/FreeBSD, which require a
// utunN/tunN naming scheme xray doesn't support here yet, see the spec's
// "Не-цели").
const vkTurnTunInterfaceName = "vkturn0"

// buildVKTurnTunConfig is buildVKTurnBridgeConfig's tun-mode counterpart:
// same vless outbound into the local client's bond listener on 127.0.0.1:9000,
// but a tun inbound instead of socks. autoOutboundsInterface must be the real
// physical uplink — without it xray's own outbound connections would route
// back through the tun device it just created (see xray-core's proxy/tun
// README, "CONSIDERATIONS" — the classic tun routing loop). routes is
// publicRoutes()'s output: the public-internet complement of the private/LAN
// CIDR list, so local devices stay reachable outside the tunnel.
func buildVKTurnTunConfig(physicalInterface string, routes []string) (string, error) {
	logLevel := "warning"
	if debugMode {
		logLevel = "debug"
	}
	routesJSON, err := json.Marshal(routes)
	if err != nil {
		return "", fmt.Errorf("buildVKTurnTunConfig: marshal routes: %w", err)
	}
	return fmt.Sprintf(`{
  "log": { "loglevel": %q },
  "inbounds": [
    {
      "protocol": "tun",
      "settings": {
        "name": %q,
        "mtu": 1500,
        "autoOutboundsInterface": %q,
        "autoSystemRoutingTable": %s
      }
    }
  ],
  "outbounds": [
    {
      "protocol": "vless",
      "settings": {
        "vnext": [
          {
            "address": "127.0.0.1",
            "port": 9000,
            "users": [
              { "id": %q, "encryption": "none" }
            ]
          }
        ]
      },
      "streamSettings": { "network": "tcp", "security": "none" }
    }
  ]
}`, logLevel, vkTurnTunInterfaceName, physicalInterface, routesJSON, vkTurnBridgeUUID), nil
}

// waitForListening polls addr with short-lived TCP dials until one succeeds
// or timeout elapses, so callers don't have to guess a fixed sleep for a
// subprocess's listener startup time. Returns ctx.Err() immediately if ctx
// is canceled while waiting (menu-driven stop during startup).
func waitForListening(ctx context.Context, addr string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	var lastErr error
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		conn, err := net.DialTimeout("tcp", addr, 200*time.Millisecond)
		if err == nil {
			conn.Close()
			return nil
		}
		lastErr = err
		if time.Now().After(deadline) {
			return fmt.Errorf("не дождался %s (%w)", addr, lastErr)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// checkVKTurnConnectivity is the positive control this project's CLAUDE.md
// "Метод" section requires for any measurement: dialing vkTurnIPCheckURL
// through the local SOCKS bridge proves actual end-to-end connectivity
// through client+xray, not just "something is listening on the port".
// Returns the outbound IP on success.
func checkVKTurnConnectivity(ctx context.Context, socksAddr string) (string, error) {
	dialer, err := proxy.SOCKS5("tcp", socksAddr, nil, proxy.Direct)
	if err != nil {
		return "", err
	}
	ctxDialer, ok := dialer.(proxy.ContextDialer)
	if !ok {
		return "", errors.New("socks-диалер не поддерживает контекст")
	}
	client := &http.Client{
		Transport: &http.Transport{DialContext: ctxDialer.DialContext},
		Timeout:   10 * time.Second,
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, vkTurnIPCheckURL, nil)
	if err != nil {
		return "", err
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("проверка IP: неожиданный статус %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 256))
	if err != nil {
		return "", err
	}
	ip := strings.TrimSpace(string(body))
	if ip == "" {
		return "", errors.New("проверка IP: пустой ответ")
	}
	return ip, nil
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

// resolveClientBin extracts the embedded client binary to binDir()
// (skipping the write when unchanged — see extractIfChanged) and returns
// its path for RunClient to exec.
func resolveClientBin() (string, error) {
	dir, err := binDir()
	if err != nil {
		return "", fmt.Errorf("resolveClientBin: %w", err)
	}
	path := filepath.Join(dir, "client"+exeSuffix())
	if err := extractIfChanged(path, embeddedClient, 0o755); err != nil {
		return "", fmt.Errorf("resolveClientBin: %w", err)
	}
	return path, nil
}

// resolveXrayBin extracts the embedded xray binary (and, on Windows, the
// wintun.dll it needs at runtime next to it — xray-core's tun inbound
// requires this, see proxy/tun's README) to binDir() and returns xray's
// path for RunXray to exec.
func resolveXrayBin() (string, error) {
	dir, err := binDir()
	if err != nil {
		return "", fmt.Errorf("resolveXrayBin: %w", err)
	}
	path := filepath.Join(dir, "xray"+exeSuffix())
	if err := extractIfChanged(path, embeddedXray, 0o755); err != nil {
		return "", fmt.Errorf("resolveXrayBin: %w", err)
	}
	if runtime.GOOS == "windows" {
		wintunPath := filepath.Join(dir, "wintun.dll")
		if err := extractIfChanged(wintunPath, embeddedWintun, 0o644); err != nil {
			return "", fmt.Errorf("resolveXrayBin: extract wintun.dll: %w", err)
		}
	}
	return path, nil
}
