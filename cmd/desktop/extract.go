package main

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
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

	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("extractIfChanged: mkdir %s: %w", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, data, perm); err != nil {
		return fmt.Errorf("extractIfChanged: write %s: %w", path, err)
	}
	if err := os.WriteFile(hashPath, []byte(hexSum), 0o600); err != nil {
		return fmt.Errorf("extractIfChanged: write hash sidecar %s: %w", hashPath, err)
	}
	return nil
}
