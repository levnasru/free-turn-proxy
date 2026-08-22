package main

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
)

// binDir returns ~/.vkturn/bin — where embedded client/xray/wintun.dll get
// extracted at startup. Same root directory already used for config.json
// (CachePath) and debug.log (debugLogPath), including the elevated-Linux
// HOME-forwarding fix already in place for tun mode's pkexec relaunch — no
// new writable-location problem to solve here.
func binDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("binDir: %w", err)
	}
	return filepath.Join(home, ".vkturn", "bin"), nil
}

// exeSuffix is ".exe" on Windows, "" elsewhere — matches the convention
// already used by the deleted resolveBin (see Task 2).
func exeSuffix() string {
	if runtime.GOOS == "windows" {
		return ".exe"
	}
	return ""
}

// extractIfChanged writes data to path only when path's current content
// differs from data, tracked via a sha256 sidecar file (<path>.sha256).
// client/xray/vkturn-desktop don't share one coherent version number to
// compare against directly (desktop is hardcoded "1.0.0", client tracks the
// repo tag, xray tracks upstream xray-core's own version) — a content hash
// of the actually-embedded bytes is the unambiguous "did anything change"
// signal instead. Skipping the write matters because these binaries are
// 15-80MB and vkturn-desktop starts often (every family member's session),
// not just once at install time.
func extractIfChanged(path string, data []byte, perm os.FileMode) error {
	sum := sha256.Sum256(data)
	hexSum := hex.EncodeToString(sum[:])
	hashPath := path + ".sha256"

	if existing, err := os.ReadFile(hashPath); err == nil && string(existing) == hexSum {
		// Cheap defensive check against external tampering/truncation of the
		// target file without the sidecar being touched: a size mismatch
		// forces a rewrite even though the sidecar claims a match. Full
		// re-hash of the on-disk file would catch more, but costs a full
		// read of a file that's about to be overwritten anyway if it's
		// wrong — not worth it for this failure mode.
		if fi, statErr := os.Stat(path); statErr == nil && fi.Size() == int64(len(data)) {
			return nil
		}
	}

	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("extractIfChanged: mkdir %s: %w", dir, err)
	}
	chownToOriginalUserIfElevated(dir)

	if err := os.WriteFile(path, data, perm); err != nil {
		return fmt.Errorf("extractIfChanged: write %s: %w", path, err)
	}
	chownToOriginalUserIfElevated(path)

	if err := os.WriteFile(hashPath, []byte(hexSum), 0o600); err != nil {
		return fmt.Errorf("extractIfChanged: write hash sidecar %s: %w", hashPath, err)
	}
	chownToOriginalUserIfElevated(hashPath)

	return nil
}

// chownToOriginalUserIfElevated restores path's ownership to the user who
// invoked pkexec, when this process is currently running as root because of
// that (see elevate_linux.go's relaunchElevated, which forwards $HOME but
// not uid/gid). Without this, files extractIfChanged writes while elevated
// (vk-turn (tun)'s pkexec relaunch) end up root-owned under the real user's
// $HOME/.vkturn/bin/ — every later non-elevated run (vk-turn (socks),
// xray-подписка) then fails there with a permission error it can't recover
// from on its own. No-op if this process isn't running as root — Windows'
// os.Geteuid() always returns -1, so this never applies there (UAC has no
// uid/gid concept to restore).
//
// ponytail: uses the caller's uid as the gid too, rather than looking up
// their actual primary group — correct on Debian/Ubuntu/Fedora's default
// useradd behavior (primary group id == uid for a normal user account),
// which covers this project's actual desktop targets; a real group lookup
// would need os/user (cgo-gated on some platforms) for a directory that's
// private (0o700) and only this app reads/writes, not worth the added
// dependency surface for that gap. Fix if this project ever needs a
// non-standard group scheme.
func chownToOriginalUserIfElevated(path string) {
	if os.Geteuid() != 0 {
		return
	}
	uidStr := os.Getenv("PKEXEC_UID")
	if uidStr == "" {
		uidStr = os.Getenv("SUDO_UID")
	}
	if uidStr == "" {
		return
	}
	uid, err := strconv.Atoi(uidStr)
	if err != nil {
		return
	}
	// Errors are deliberately swallowed here: this is a best-effort
	// convenience restore, not a security boundary, and the caller
	// (extractIfChanged) has already done its real job (the file exists
	// with the right content) — failing the whole extraction over a
	// chown that doesn't matter on a platform/filesystem that doesn't
	// support it (e.g. some container setups) would be worse than a
	// silently-skipped ownership fix.
	_ = os.Chown(path, uid, uid)
}
