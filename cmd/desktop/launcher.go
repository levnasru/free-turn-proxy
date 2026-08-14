// cmd/desktop/launcher.go
package main

import (
	"context"
	"io"
	"os/exec"
	"strconv"
	"strings"
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
