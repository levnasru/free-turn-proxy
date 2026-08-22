# Desktop Single-Binary Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Collapse `vkturn-desktop`'s three-file distribution (`client`, `vkturn-desktop`, `xray`, plus `wintun.dll` on Windows) into one file per platform via `go:embed`, without changing any runtime behavior — the subprocess model (`RunClient`/`RunXray`, process isolation) stays exactly as it is today.

**Architecture:** `client` and `xray` binaries get built and copied into `cmd/desktop/embedded/<goos>_<goarch>/` before `cmd/desktop` is compiled; `//go:embed` directives (in new platform-tagged files, matching this package's existing `tray.go`/`tray_other.go` convention) bake those bytes into `vkturn-desktop` itself. At startup, `resolveClientBin`/`resolveXrayBin` extract the embedded bytes to `~/.vkturn/bin/` (skipping the write via a sha256-sidecar check when content is unchanged from a prior run) and hand the extracted path to the unmodified `RunClient`/`RunXray`.

**Tech Stack:** Go stdlib only (`embed`, `crypto/sha256`) — no new dependencies.

**Spec:** `docs/superpowers/specs/2026-08-22-desktop-single-binary-design.md`

## Global Constraints

- No local Go toolchain in this environment — every `go build`/`go test`/`go vet`/`gofmt` command runs inside Docker: `docker run --rm -v "$PWD":/src -w /src -v vkturn-gomodcache:/go/pkg/mod golang:latest <command>`. Pass `-buildvcs=false` to build/test/vet.
- `cmd/desktop`'s full build/vet/test needs the systray cgo toolchain: `apt-get update -qq && apt-get install -y -qq libayatana-appindicator3-dev libgtk-3-dev pkg-config gcc` inside the same docker invocation, then `CGO_ENABLED=1 go build ...`.
- House test style: plain stdlib `testing`, table/literal assertions — no testify. See `cmd/desktop/launcher_test.go`, `cmd/desktop/lanexclude_test.go` for the pattern.
- Platform-specific code goes in `_linux.go`/`_windows.go` files, build-tag-by-filename (matches `tray.go`/`tray_other.go`, `netroute_linux.go`/`netroute_windows.go`, `elevate_linux.go`/`elevate_windows.go` already in this package).
- **`go:embed` requires the referenced file to exist on disk at build time** — this repo's own `cmd/desktop` package, and any CI/task in this workflow that runs a plain `go build ./...`/`go test ./...` across the whole repo, would break the moment embed directives land pointing at a gitignored, empty directory. Resolution (a deliberate, documented deviation from the spec's "gitignored, никогда не коммитится" line — the spec's *goal* is unchanged, only this housekeeping mechanic): commit small placeholder files at the exact 5 embed paths (a few lines of text each, not real binaries), so a fresh clone always builds. The real CI build (Task 3) overwrites these placeholders with real binaries in the ephemeral GitHub Actions workspace — it never runs `git add`/`git commit`, so there's no risk of a real 30MB binary landing in a CI-driven commit. The only residual risk is a **local** developer running a full two-stage build by hand and then carelessly `git add -A`-ing the result — call this out explicitly in Task 2's README and don't otherwise engineer around it (matches this project's standing "review `git status` before committing" discipline).
- Commit after each task, not after each step.

---

### Task 1: `extractIfChanged` — content-hash-gated file extraction

**Files:**
- Create: `cmd/desktop/extract.go`
- Test: `cmd/desktop/extract_test.go`

**Interfaces:**
- Produces: `extractIfChanged(path string, data []byte, perm os.FileMode) error`, `binDir() (string, error)`, `exeSuffix() string`. Used by Task 2's rewritten `resolveClientBin`/`resolveXrayBin`.

- [ ] **Step 1: Write the failing tests**

```go
// cmd/desktop/extract_test.go
package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestExtractIfChangedWritesNewFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sub", "client")
	data := []byte("fake binary content v1")

	if err := extractIfChanged(path, data, 0o755); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading extracted file: %v", err)
	}
	if string(got) != string(data) {
		t.Fatalf("content = %q, want %q", got, data)
	}

	if _, err := os.ReadFile(path + ".sha256"); err != nil {
		t.Fatalf("expected sidecar hash file, got error: %v", err)
	}
}

func TestExtractIfChangedSkipsUnchangedContent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "client")
	data := []byte("fake binary content v1")

	if err := extractIfChanged(path, data, 0o755); err != nil {
		t.Fatalf("first extract: %v", err)
	}
	first, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat after first extract: %v", err)
	}

	// Force a distinguishable mtime so a real rewrite would be detectable —
	// otherwise two writes fast enough could land on the same mtime and the
	// test would pass even if extractIfChanged wrongly rewrote.
	past := time.Now().Add(-time.Hour)
	if err := os.Chtimes(path, past, past); err != nil {
		t.Fatalf("chtimes: %v", err)
	}

	if err := extractIfChanged(path, data, 0o755); err != nil {
		t.Fatalf("second extract: %v", err)
	}
	second, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat after second extract: %v", err)
	}
	if !second.ModTime().Equal(past) {
		t.Fatalf("file was rewritten on unchanged content: mtime = %v, want unchanged %v (original before chtimes: %v)", second.ModTime(), past, first.ModTime())
	}
}

func TestExtractIfChangedRewritesOnDifferentContent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "client")

	if err := extractIfChanged(path, []byte("content v1"), 0o755); err != nil {
		t.Fatalf("first extract: %v", err)
	}
	if err := extractIfChanged(path, []byte("content v2, longer than v1"), 0o755); err != nil {
		t.Fatalf("second extract: %v", err)
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading extracted file: %v", err)
	}
	if string(got) != "content v2, longer than v1" {
		t.Fatalf("content = %q, want updated content", got)
	}
}

func TestExtractIfChangedRewritesWhenFileMissingButSidecarPresent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "client")
	data := []byte("fake binary content")

	if err := extractIfChanged(path, data, 0o755); err != nil {
		t.Fatalf("first extract: %v", err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatalf("removing target file: %v", err)
	}
	// sidecar hash file is deliberately left behind — simulates a user
	// deleting the extracted binary but not the sidecar.

	if err := extractIfChanged(path, data, 0o755); err != nil {
		t.Fatalf("re-extract after deletion: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("expected file to be recreated, stat error: %v", err)
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `docker run --rm -v "$PWD":/src -w /src -v vkturn-gomodcache:/go/pkg/mod golang:latest go test -buildvcs=false ./cmd/desktop/... -run TestExtractIfChanged -v`
Expected: FAIL — `extractIfChanged` undefined.

- [ ] **Step 3: Write the implementation**

```go
// cmd/desktop/extract.go
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
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `docker run --rm -v "$PWD":/src -w /src -v vkturn-gomodcache:/go/pkg/mod golang:latest go test -buildvcs=false ./cmd/desktop/... -run TestExtractIfChanged -v`
Expected: PASS (all 4 tests).

- [ ] **Step 5: gofmt and commit**

```bash
docker run --rm -v "$PWD":/src -w /src -v vkturn-gomodcache:/go/pkg/mod golang:latest gofmt -l cmd/desktop/extract.go cmd/desktop/extract_test.go
git add cmd/desktop/extract.go cmd/desktop/extract_test.go
git commit -m "feat(desktop): extractIfChanged — content-hash-gated binary extraction"
```

---

### Task 2: Embed scaffolding + rewire `resolveClientBin`/`resolveXrayBin`

**Files:**
- Create: `cmd/desktop/embedded/README.md`
- Create: `cmd/desktop/embedded/linux_amd64/client` (placeholder)
- Create: `cmd/desktop/embedded/linux_amd64/xray` (placeholder)
- Create: `cmd/desktop/embedded/windows_amd64/client.exe` (placeholder)
- Create: `cmd/desktop/embedded/windows_amd64/xray.exe` (placeholder)
- Create: `cmd/desktop/embedded/windows_amd64/wintun.dll` (placeholder)
- Create: `cmd/desktop/embed_linux.go` (`//go:build linux`)
- Create: `cmd/desktop/embed_windows.go` (`//go:build windows`)
- Modify: `cmd/desktop/launcher.go` (delete `resolveBin`, rewrite `resolveClientBin`/`resolveXrayBin`, currently at `cmd/desktop/launcher.go:350-394`)
- Modify: `cmd/desktop/main.go` (`runMode` at `:215-238`, `runVKTurnMode` signature+call at `:247-253`, `runXraySubscriptionMode` signature+call at `:325`,`:372`)
- Modify: `cmd/desktop/tun.go` (`runVKTurnTunMode` signature+calls at `:22`,`:60`,`:65`)

**Interfaces:**
- Consumes: `extractIfChanged`, `binDir`, `exeSuffix` (Task 1).
- Produces: `resolveClientBin() (string, error)`, `resolveXrayBin() (string, error)` — same names, **dropped `dir` parameter**. `runVKTurnMode`, `runVKTurnTunMode`, `runXraySubscriptionMode` all drop their `dir string` parameter too (nothing else in those functions used it).

- [ ] **Step 1: Write the embedded placeholder files and README**

```bash
mkdir -p cmd/desktop/embedded/linux_amd64 cmd/desktop/embedded/windows_amd64
```

`cmd/desktop/embedded/README.md`:
```markdown
# cmd/desktop/embedded/

`go:embed`-source for `vkturn-desktop`'s single-binary distribution (see
`docs/superpowers/specs/2026-08-22-desktop-single-binary-design.md`).

The five files under `linux_amd64/` and `windows_amd64/` here are **tracked
placeholders**, not real binaries — `go:embed` requires the referenced file
to exist at build time, so a fresh clone needs *something* here or every
`go build ./...`/`go test ./...` in this repo breaks. `.github/workflows/release.yml`
overwrites them with the real `client`/`xray`/`wintun.dll` builds before the
release build of `cmd/desktop` runs, in its own ephemeral CI workspace.

**Never `git add`/commit a locally-overwritten real binary here.** If you run
a full two-stage build by hand for local testing, check `git status` before
committing anything — a real `client`/`xray` here is 15-33MB and does not
belong in this repository's history.
```

`cmd/desktop/embedded/linux_amd64/client`:
```
placeholder — overwritten by .github/workflows/release.yml before the real
desktop build. See cmd/desktop/embedded/README.md. Do not execute this file.
```

`cmd/desktop/embedded/linux_amd64/xray`: same placeholder text, one line changed to name `xray` instead of `client`.

`cmd/desktop/embedded/windows_amd64/client.exe`: same placeholder text, naming `client.exe`.

`cmd/desktop/embedded/windows_amd64/xray.exe`: same placeholder text, naming `xray.exe`.

`cmd/desktop/embedded/windows_amd64/wintun.dll`: same placeholder text, naming `wintun.dll`, noting it's a third-party driver DLL (from wintun.net) not our code.

- [ ] **Step 2: Write the platform embed files**

```go
// cmd/desktop/embed_linux.go
//go:build linux

package main

import _ "embed"

//go:embed embedded/linux_amd64/client
var embeddedClient []byte

//go:embed embedded/linux_amd64/xray
var embeddedXray []byte

// embeddedWintun is nil on Linux — xray's tun inbound only needs wintun.dll
// on Windows (see xray-core's proxy/tun README). Declared here too (not just
// in embed_windows.go) so resolveXrayBin in launcher.go can reference it
// unconditionally without a second build-tag split.
var embeddedWintun []byte
```

```go
// cmd/desktop/embed_windows.go
//go:build windows

package main

import _ "embed"

//go:embed embedded/windows_amd64/client.exe
var embeddedClient []byte

//go:embed embedded/windows_amd64/xray.exe
var embeddedXray []byte

//go:embed embedded/windows_amd64/wintun.dll
var embeddedWintun []byte
```

- [ ] **Step 3: Rewrite `resolveClientBin`/`resolveXrayBin`, delete `resolveBin`**

In `cmd/desktop/launcher.go`, delete the entire block from the `resolveClientBin` doc comment through the end of `resolveBin` (currently lines 350-394 — the exact range from `// resolveClientBin locates...` through the closing `}` of `resolveBin`), and replace it with:

```go
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
```

Check `cmd/desktop/launcher.go`'s imports after this edit: `runtime` is still used (by the new `runtime.GOOS == "windows"` check); if `filepath` or `fmt` were only used by the deleted `resolveBin`, confirm they're still imported and used elsewhere in the file (both are — `filepath.Join` and `fmt.Errorf`/`fmt.Sprintf` appear in other functions in this file, e.g. `buildVKTurnBridgeConfig`, `buildVKTurnTunConfig`). `exec` may become unused if `resolveBin`'s `exec.LookPath` was its only use in this file — check: `exec.CommandContext` is used by both `RunXray` and (indirectly via `RunClient`, same file) — still needed, no import to remove.

- [ ] **Step 4: Update the three call sites and drop `dir` threading**

In `cmd/desktop/main.go`:

`runMode` (currently lines 215-238) — remove the `os.Executable()`/`dir` computation entirely and the `dir` argument from all three calls:

```go
func runMode(cfg *DesktopConfig, mode string) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() { <-sigCh; cancel() }()
	defer signal.Stop(sigCh)

	switch mode {
	case "vk-turn":
		runVKTurnMode(ctx, cancel, cfg)
	case "vk-turn-tun":
		runVKTurnTunMode(ctx, cancel, cfg)
	case "xray":
		runXraySubscriptionMode(ctx, cancel, cfg)
	}
}
```

`runVKTurnMode`'s signature (line 247) changes from
`func runVKTurnMode(ctx context.Context, cancel context.CancelFunc, dir string, cfg *DesktopConfig) {`
to
`func runVKTurnMode(ctx context.Context, cancel context.CancelFunc, cfg *DesktopConfig) {`
— and its body's `resolveClientBin(dir)`/`resolveXrayBin(dir)` (lines 248, 253) become `resolveClientBin()`/`resolveXrayBin()`.

`runXraySubscriptionMode`'s signature (line 325) changes the same way, and its `resolveXrayBin(dir)` call (line 372) becomes `resolveXrayBin()`.

Check `cmd/desktop/main.go`'s imports: `filepath` was used for `filepath.Dir(exePath)` in the deleted lines — if that was `filepath`'s only use in this file, remove the import; if `os.Executable()` was `os`'s only remaining justification anywhere in this file check `os` is still used elsewhere (it is — `os.Stderr`, `os.Exit` etc. appear throughout `main.go`, keep the import).

In `cmd/desktop/tun.go`:

`runVKTurnTunMode`'s signature (line 22) changes from
`func runVKTurnTunMode(ctx context.Context, cancel context.CancelFunc, dir string, cfg *DesktopConfig) {`
to
`func runVKTurnTunMode(ctx context.Context, cancel context.CancelFunc, cfg *DesktopConfig) {`
— and its `resolveClientBin(dir)`/`resolveXrayBin(dir)` calls (lines 60, 65) become `resolveClientBin()`/`resolveXrayBin()`.

- [ ] **Step 5: Build, vet, and test the full package**

```bash
docker run --rm -v "$PWD":/src -w /src -v vkturn-gomodcache:/go/pkg/mod golang:latest bash -c "
apt-get update -qq && apt-get install -y -qq libayatana-appindicator3-dev libgtk-3-dev pkg-config gcc >/dev/null
gofmt -l cmd/desktop/
CGO_ENABLED=1 go build -buildvcs=false ./cmd/desktop/...
go vet -buildvcs=false ./cmd/desktop/...
go test -buildvcs=false ./cmd/desktop/... -v
GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go build -buildvcs=false -o /dev/null ./cmd/desktop/...
"
```

Expected: gofmt silent, build/vet/Windows-cross-compile clean, all existing tests (Tasks 1-7 of the prior TUN plan, plus this task's Task 1) still pass.

- [ ] **Step 6: Commit**

```bash
git add cmd/desktop/embedded/ cmd/desktop/embed_linux.go cmd/desktop/embed_windows.go cmd/desktop/launcher.go cmd/desktop/main.go cmd/desktop/tun.go
git commit -m "feat(desktop): embed client+xray via go:embed, drop filesystem-adjacent binary lookup"
```

---

### Task 3: CI two-stage build wiring

**Files:**
- Modify: `.github/workflows/release.yml` (near the existing "Build xray CLI" / "Fetch wintun.dll" steps, currently `:176-195`)
- Modify: `.goreleaser.yaml` (`release.extra_files`, currently listing `xray-dist/*`/would list `wintun-dist/wintun.dll`)
- Modify: `docs/desktop.md`, `docs/flags.md` (kit-assembly instructions — single file now, no more manual xray/wintun.dll placement)

**Interfaces:**
- No Go interfaces — CI/YAML/docs only. Consumes Task 2's `cmd/desktop/embedded/<goos>_<goarch>/` layout as the destination for real build output.

This task has no automated test (CI YAML, unverifiable outside a real GitHub Actions run) — verification is YAML parsing plus a manual reasoning check per step below, same as Task 7 of the prior TUN plan.

- [ ] **Step 1: Add a "Build client for desktop embed" step**

In `.github/workflows/release.yml`, add a new step immediately before the existing "Build xray CLI" step (same job, same runner — `ubuntu-latest`, already has the Go toolchain set up by an earlier step in this job):

```yaml
      - name: Build client for desktop embed
        run: |
          mkdir -p cmd/desktop/embedded/linux_amd64 cmd/desktop/embedded/windows_amd64
          CGO_ENABLED=0 GOOS=linux   GOARCH=amd64 go build -trimpath -ldflags "-s -w -checklinkname=0" -o cmd/desktop/embedded/linux_amd64/client        ./cmd/client
          CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build -trimpath -ldflags "-s -w -checklinkname=0" -o cmd/desktop/embedded/windows_amd64/client.exe ./cmd/client
```

(This duplicates a small amount of build work already done elsewhere by goreleaser's own multi-platform `client` build id — accepted trade-off to avoid coordinating build-graph ordering between goreleaser build ids, which goreleaser doesn't support natively. `client` is pure Go and builds in seconds.)

- [ ] **Step 2: Extend the existing xray/wintun steps to also populate `cmd/desktop/embedded/`**

Immediately after the existing "Build xray CLI" step's `run:` block (after the four `CGO_ENABLED=0 GOOS=... go build ...` lines, still inside that same step), add:

```yaml
          cp xray-dist/xray-linux-amd64        cmd/desktop/embedded/linux_amd64/xray
          cp xray-dist/xray-windows-amd64.exe  cmd/desktop/embedded/windows_amd64/xray.exe
```

Immediately after the existing "Fetch wintun.dll" step's `run:` block (after the `unzip -p ... > wintun-dist/wintun.dll` line, still inside that same step), add:

```yaml
          cp wintun-dist/wintun.dll cmd/desktop/embedded/windows_amd64/wintun.dll
```

- [ ] **Step 3: Verify ordering — all three steps must run before `goreleaser-action`**

Read the full job in `.github/workflows/release.yml` from "Build client for desktop embed" through the `goreleaser/goreleaser-action@v7.2.2` step and confirm the order is: build client → build xray CLI (+ copy) → fetch wintun.dll (+ copy) → goreleaser-action. If any of these three steps currently appears after `goreleaser-action` in the file, move it before. This ordering is load-bearing — `cmd/desktop/embedded/` must hold real binaries before goreleaser's own `go build ./cmd/desktop` runs, or the release build silently embeds the Task 2 placeholders.

- [ ] **Step 4: Check whether `xray-dist`/`wintun-dist` are still needed as loose release assets**

Run, from the repo root (not in Docker — this is a plain text search, no Go toolchain needed):

```bash
grep -rn "xray-dist\|wintun-dist" --include="*.yml" --include="*.yaml" --include="*.md" --include="*.sh" . 2>/dev/null
```

Read every hit. If `.goreleaser.yaml`'s `release.extra_files` is the *only* place `xray-dist/xray-windows-amd64.exe` etc. and `wintun-dist/wintun.dll` are referenced (besides the `release.yml` build steps that produce them, which stay regardless since Step 2 above now consumes their output too), remove those specific entries from `release.extra_files` — they were added for the desktop kit's previous three-file distribution and now have no consumer. Do **not** remove `xray-dist/xray-darwin-amd64`/`xray-dist/xray-darwin-arm64` entries if present and referenced elsewhere (macOS desktop packaging is out of scope for this plan per the spec's Не-цели — check `docs/superpowers/specs/2026-08-14-desktop-client-design.md` before touching anything darwin-related). If the grep turns up any other consumer (a script, a doc link), leave the corresponding `extra_files` entry in place and note why in your task report instead of removing it.

- [ ] **Step 5: Validate YAML**

```bash
python3 -c "import yaml; yaml.safe_load(open('.github/workflows/release.yml')); yaml.safe_load(open('.goreleaser.yaml')); print('valid')"
```

If `python3`/`PyYAML` isn't available, note that in your report rather than skipping validation silently — a syntax error in `release.yml` only surfaces on the next real CI run otherwise.

- [ ] **Step 6: Update `docs/desktop.md` and `docs/flags.md`**

Read both files in full. In `docs/desktop.md`, remove or rewrite any instruction that describes placing `client`/`xray`/`wintun.dll` next to `vkturn-desktop` manually (a single downloaded `vkturn-desktop`/`vkturn-desktop.exe` is now sufficient — the app extracts what it needs to `~/.vkturn/bin/` on first run). In `docs/flags.md`, remove the `wintun.dll` release-asset-filename note added earlier today (no longer a separate downloadable asset for the desktop kit).

- [ ] **Step 7: Commit**

```bash
git add .github/workflows/release.yml .goreleaser.yaml docs/desktop.md docs/flags.md
git commit -m "ci(desktop): build client+xray+wintun.dll into cmd/desktop/embedded/ before the release build"
```

---

### Task 4: Manual golden-path verification

**Files:** None (verification only — no code changes).

This task has no automated test target; it's the live check that the two-stage build actually produces a working single file, matching the prior TUN plan's Task 5 verification style.

- [ ] **Step 1: Real two-stage local build**

```bash
cd /path/to/repo  # a clean checkout or worktree, not one with leftover real binaries from earlier manual testing
docker run --rm -v "$PWD":/src -w /src -v vkturn-gomodcache:/go/pkg/mod golang:latest bash -c "
apt-get update -qq && apt-get install -y -qq libayatana-appindicator3-dev libgtk-3-dev pkg-config gcc >/dev/null
mkdir -p cmd/desktop/embedded/linux_amd64
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags '-s -w -checklinkname=0' -o cmd/desktop/embedded/linux_amd64/client ./cmd/client
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags '-s -w' -o cmd/desktop/embedded/linux_amd64/xray github.com/xtls/xray-core/main
CGO_ENABLED=1 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags '-s -w -X main.version=debug-test' -o /out-vkturn-desktop ./cmd/desktop
"
```

(mount an output path for the final binary, e.g. add `-v /tmp/out:/out-parent` and adjust `-o` accordingly — the exact mount mechanics follow the same pattern used for every prior build in this session, adapt as needed.)

- [ ] **Step 2: Confirm no loose `client`/`xray` are needed alongside**

```bash
mkdir -p /tmp/vkturn-single-test && cp <the built vkturn-desktop binary> /tmp/vkturn-single-test/vkturn-desktop
cd /tmp/vkturn-single-test
ls  # should show ONLY vkturn-desktop — nothing else
```

- [ ] **Step 3: Run all three menu modes, confirm extraction happens**

Run `./vkturn-desktop`, log in, pick `vk-turn (socks)` (or whichever mode has valid test credentials available). Confirm:
- `~/.vkturn/bin/client`, `~/.vkturn/bin/client.sha256`, `~/.vkturn/bin/xray`, `~/.vkturn/bin/xray.sha256` now exist.
- The session connects exactly as it did before this plan (same behavior as the prior TUN plan's golden-path check — positive-control IP check passes).

- [ ] **Step 4: Confirm skip-on-rerun behavior**

```bash
stat -c '%Y' ~/.vkturn/bin/client ~/.vkturn/bin/xray   # note the mtimes
```

Run `./vkturn-desktop` again, pick the same mode, let it connect, then Ctrl+C. Re-check:

```bash
stat -c '%Y' ~/.vkturn/bin/client ~/.vkturn/bin/xray   # should be UNCHANGED from the first run
```

If either mtime changed, `extractIfChanged` is rewriting when it shouldn't — this is a real regression, not a minor finding; stop and investigate before considering this plan done.

- [ ] **Step 5: Report**

No commit for this task (verification only). Record the result in the SDD ledger / task report: pass/fail for each of steps 2-4, and the binary size of the final single `vkturn-desktop` (expected roughly client+xray's combined size on top of today's `vkturn-desktop` size — confirms the embed actually happened, not just compiled against empty placeholders).

---

## Self-Review Notes

- **Spec coverage:** two-stage build (Task 3) ✓, `go:embed` platform files (Task 2) ✓, `~/.vkturn/bin/` extraction reusing the existing writable-dir precedent (Task 1/2) ✓, sha256-sidecar skip-rewrite behavior (Task 1, added per user feedback after initial spec approval) ✓, `resolveClientBin`/`resolveXrayBin` becoming the only touched call sites with `RunClient`/`RunXray` unchanged (Task 2) ✓, distribution outcome / extra_files cleanup (Task 3) ✓, testing plan incl. manual golden-path and skip-on-rerun check (Task 4) ✓, open risk about placeholder-vs-gitignore resolved explicitly in Global Constraints (a real deviation from the spec's literal "никогда не коммитится" line, documented rather than silently done) ✓. Antivirus/SmartScreen risk and CI-ordering risk from the spec's "Открытые риски" are addressed by Task 3 Step 3 (explicit ordering check) and flagged for live-Windows testing (out of reach in this Linux-only environment, same caveat as the prior plan's Windows-only code).
- **Placeholder scan:** no TBD/TODO; every step has real, complete code or an exact shell command.
- **Type consistency:** `resolveClientBin() (string, error)` / `resolveXrayBin() (string, error)` signatures consistent between Task 2's Step 3 (definition) and Step 4 (call sites). `extractIfChanged(path string, data []byte, perm os.FileMode) error` consistent between Task 1 (definition+tests) and Task 2 (call sites in the rewritten resolve* functions). `embeddedClient`/`embeddedXray`/`embeddedWintun` `[]byte` vars consistent between Task 2's embed files and their use in `launcher.go`.
