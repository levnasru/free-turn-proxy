# VK-TURN Desktop Client — Milestone A Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Family members log in (username/password) from a terminal `vkturn-desktop`
binary on Windows/Linux and get either a vk-turn tunnel or an xray-subscription
SOCKS proxy running, with zero manual config-file handling. No admin/root rights,
no GUI — arrow-key+Enter terminal menu. WG full-tunnel is Milestone B, not here.

**Architecture:** Extend the already-live `vkturn-ios-portal` (bcrypt auth, HMAC
sessions, public on `lft.levnas.ru`) with a small bearer-token JSON API
(`/api/v1/login`, `/api/v1/config`). `panel.js` gets a `device=desktop` option
that provisions portal users the same way it already does for iOS. A new
`cmd/desktop` Go binary in this repo logs in, caches the config locally, and
spawns either the existing `client` binary (vk-turn mode) or `xray` (subscription
mode) as a subprocess — no in-process reuse of `cmd/client`'s `main()`, which
isn't factored for that and uses `os.Exit` throughout; subprocess is simpler,
safer, and matches how `xray` is already spawned in the current manual kits.

**Tech Stack:** Go 1.26 both sides. `golang.org/x/crypto/bcrypt` (already a
portal dependency). `golang.org/x/term` (new, for password-no-echo + raw-mode
menu). `github.com/xtls/libXray` (new, share-link → xray JSON conversion —
same library Android already uses via JNI, here called directly as Go).

**Spec:** `docs/superpowers/specs/2026-08-14-desktop-client-design.md`

## Global Constraints

- **No local Go toolchain on this machine — every `go`/`gofmt` command in every
  task below must run through Docker, not bare.** Two fixed wrapper forms
  (note: literal absolute paths only, never `$PWD`/`$HOME` — shell variable
  expansion inside the mount flags gets rejected by this sandbox):
  - `-u 1000:1001 -e HOME=/tmp` is required, not optional: without it the
    container runs as root, and anything it writes into the bind-mounted
    source (`go.mod`/`go.sum` after `go get`, any generated file) becomes
    root-owned on the host — verified live on 2026-08-14 — which then blocks
    your own (non-root) `git add`/edit of those same files. `-e HOME=/tmp`
    gives uid 1000 a writable `$HOME` inside the container (it has none by
    default), which `go` needs for its build cache.
  - **This repo** (`cmd/desktop`, `.goreleaser.yaml` work). The worktree's
    fixed absolute path is
    `/home/lev/free-turn-proxy-levnasru/free-turn-proxy/.claude/worktrees/desktop-client-milestone-a`
    — every `Run:` line below already has it inlined, but if you construct a
    new command, use exactly this path, not `pwd` output re-derived some other
    way:
    ```
    docker run --rm -u 1000:1001 -e HOME=/tmp \
      -v /home/lev/free-turn-proxy-levnasru/free-turn-proxy/.claude/worktrees/desktop-client-milestone-a:/src \
      -w /src -v /home/lev/go:/go -e GOFLAGS=-buildvcs=false -e GOPROXY=direct golang:latest \
      sh -c '<command>'
    ```
  - **Portal** (`/home/lev/vkturn-ios-portal`, Tasks 1-4):
    ```
    docker run --rm -u 1000:1001 -e HOME=/tmp -v /home/lev/vkturn-ios-portal:/src -w /src -v /home/lev/go:/go \
      -e GOFLAGS=-buildvcs=false -e GOPROXY=direct golang:latest \
      sh -c '<command>'
    ```
  - Cross-compiling (Task 9's `GOOS=windows`/`GOOS=linux` builds): add
    `-e GOOS=<os> -e GOARCH=amd64 -e CGO_ENABLED=0` to either form's `docker run`
    flags — do not set `GOOS`/`GOARCH` as a bare shell prefix, it has no effect
    outside the container. If the build output path (`-o ...`) needs to land
    outside `/src` (Task 9 writes to `/tmp` for the live smoke test), also add
    `-v /tmp:/tmp` — otherwise the file lands in the container's ephemeral
    `/tmp` and is lost when `--rm` tears the container down.
  - Every `Run:` line in this plan already shows the fully-wrapped command —
    this was verified working live on 2026-08-14 (`go build ./...`,
    `go test ./...`, `gofmt -l .`, a `GOOS=windows` cross-compile, and
    host-side file ownership all confirmed correct through this exact wrapper,
    module cache reused via the `/go` mount).
- Portal lives at `/home/lev/vkturn-ios-portal/main.go` (single `package main`,
  no git repo there — edit in place, no VCS ceremony expected). Deployed by
  `scp`-ing the built binary to VPS-NL (`vps` SSH alias, `89.124.71.77`) and
  `systemctl restart vkturn-ios-portal`. **Never** touch the live
  `/etc/vkturn/ios-portal/config.json` or `users.json` on the VPS directly —
  only through `cliAddDesktopUser`/tested code paths.
- `panel.js` lives at `/home/lev/vkturn-android-kit/panel.js` on `ai-server`
  (reach via `ssh -L 8088:127.0.0.1:8088 -N ai-lan` per project memory, or edit
  directly via `ssh ai-lan`). It is a **loopback-only, zero-auth admin tool** —
  do not change that bind or add auth to it; the new public surface lives
  entirely in the portal, not here.
- This repo's fork discipline (see `CLAUDE.md`): the only sanctioned upstream
  patch points are `internal/config/config.go`, `cmd/client/main.go`,
  `mobile/mobile.go`. `cmd/desktop` is a **new** directory, not a patch to any
  of those three — no upstream file gets touched by this plan.
- Secrets: never commit `panel-secrets.json`, `users.json`, `cookie_secret`,
  hub tokens, or any live credential value. Test fixtures use obviously-fake
  values (`"tok123"`, `"pin123"`, etc.).
- After every Go change in this repo: `gofmt -l .` and `go build ./...` must be
  clean before moving to the next task.

---

## File Structure

**Portal (`/home/lev/vkturn-ios-portal/`):**
- Modify `main.go` — `Config`/`User` struct new fields, `cliAddUser` split
  (existing one untouched, new `cliAddDesktopUser` added), `main()` CLI/route
  wiring.
- Create `api.go` — bearer-token auth helper + `/api/v1/login` +
  `/api/v1/config` handlers + `DesktopConfig` JSON type.
- Create `main_test.go` — first test file in this project; covers the new
  `Config`/`User` fields round-tripping through `loadConfig`/`loadUsers`.
- Create `api_test.go` — handler tests via `httptest`.
- Modify `config.example.json` — document the two new fields.

**panel.js (`/home/lev/vkturn-android-kit/panel.js`):**
- Modify — new `device=desktop` branch mirroring the existing `device=ios`
  branch (`genIosArtifact`/SSH `adduser` call).

**This repo (`cmd/desktop/`, new):**
- Create `cmd/desktop/config.go` — `DesktopConfig` type (byte-for-byte the same
  shape as the portal's, kept as a separate copy since these are two different
  Go modules with no shared package — see Task 6 for the exact fields).
- Create `cmd/desktop/apiclient.go` — login, config fetch, local cache
  (`~/.vkturn/config.json`, `0600`).
- Create `cmd/desktop/apiclient_test.go`.
- Create `cmd/desktop/menu.go` — arrow-key+Enter terminal menu, key-parsing
  logic split out as a pure, testable function.
- Create `cmd/desktop/menu_test.go`.
- Create `cmd/desktop/launcher.go` — argv/env builders for both subprocess
  modes (pure, testable) + thin `os/exec` runners.
- Create `cmd/desktop/launcher_test.go`.
- Create `cmd/desktop/main.go` — wires login → menu → launcher.
- Modify `.goreleaser.yaml` — add `id: desktop` build (`windows_amd64`,
  `linux_amd64`) to the existing `raw` archive.

---

### Task 1: Portal — `Config`/`User` new fields

**Files:**
- Modify: `/home/lev/vkturn-ios-portal/main.go:45-60` (`Config` struct),
  `main.go:86-93` (`User` struct)
- Modify: `/home/lev/vkturn-ios-portal/config.example.json`
- Test: `/home/lev/vkturn-ios-portal/main_test.go` (new file)

**Interfaces:**
- Produces: `Config.HubHost, Config.HubToken, Config.HubPin string`;
  `User.DesktopAccounts []int`, `User.DesktopStreams int`,
  `User.SplitMode string`, `User.XraySubID string` (all `omitempty` on disk).

- [ ] **Step 1: Write the failing test**

```go
// main_test.go
package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadConfigDesktopFields(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	body := `{
	  "listen": "127.0.0.1:8449",
	  "cert_file": "/tmp/c.crt", "key_file": "/tmp/c.key",
	  "cookie_secret": "00112233445566778899aabbccddeeff00112233445566778899aabbccddee",
	  "obf_key": "abc", "peer_address": "1.2.3.4:56000",
	  "wg_iface": "wgcl", "wg_server_pubkey": "pub", "wg_conf_path": "/tmp/wg.conf",
	  "wg_subnet_prefix": "10.13.13.", "pool_dir": "/tmp/pool", "users_file": "/tmp/users.json",
	  "hub_host": "89.124.71.77", "hub_token": "tok123", "hub_pin": "pin123"
	}`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := loadConfig(path)
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if cfg.HubHost != "89.124.71.77" || cfg.HubToken != "tok123" || cfg.HubPin != "pin123" {
		t.Fatalf("hub fields not loaded: %+v", cfg)
	}
}

func TestUserDesktopFieldsRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "users.json")
	users := []User{{
		Username: "vasya", PasswordHash: "h", AccountID: "acct1",
		DesktopAccounts: []int{8445, 8446}, DesktopStreams: 12,
		SplitMode: "exclude", XraySubID: "sub-abc123",
	}}
	if err := saveUsers(path, users); err != nil {
		t.Fatal(err)
	}
	got, err := loadUsers(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].XraySubID != "sub-abc123" || len(got[0].DesktopAccounts) != 2 ||
		got[0].DesktopStreams != 12 || got[0].SplitMode != "exclude" {
		t.Fatalf("round-trip mismatch: %+v", got)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `docker run --rm -u 1000:1001 -e HOME=/tmp -v /home/lev/vkturn-ios-portal:/src -w /src -v /home/lev/go:/go -e GOFLAGS=-buildvcs=false -e GOPROXY=direct golang:latest sh -c "go test ./... -run 'TestLoadConfigDesktopFields|TestUserDesktopFieldsRoundTrip' -v"`
Expected: FAIL (compile error — `HubHost`/`HubToken`/`HubPin`/`DesktopAccounts`/
`DesktopStreams`/`SplitMode`/`XraySubID` undefined).

- [ ] **Step 3: Add the fields**

In `Config` (after `PeerAddress` line):
```go
	HubHost        string `json:"hub_host"`  // "89.124.71.77" — same VPS as PeerAddress
	HubToken       string `json:"hub_token"` // matches S.hubToken in panel-secrets.json
	HubPin         string `json:"hub_pin"`   // matches S.hubPin in panel-secrets.json
```

In `User` (after `WGPubkey` line):
```go
	DesktopAccounts []int  `json:"desktop_accounts,omitempty"` // hub ports, e.g. [8445,8446]
	DesktopStreams  int    `json:"desktop_streams,omitempty"`
	SplitMode       string `json:"split_mode,omitempty"`
	XraySubID       string `json:"xray_sub_id,omitempty"` // subId of an existing x-ui client
```

Add to `config.example.json` (after `"peer_address"`):
```json
  "hub_host": "89.124.71.77",
  "hub_token": "REPLACE_WITH_PANEL_HUB_TOKEN",
  "hub_pin": "REPLACE_WITH_PANEL_HUB_PIN",
```

- [ ] **Step 4: Run test to verify it passes**

Run: `docker run --rm -u 1000:1001 -e HOME=/tmp -v /home/lev/vkturn-ios-portal:/src -w /src -v /home/lev/go:/go -e GOFLAGS=-buildvcs=false -e GOPROXY=direct golang:latest sh -c 'go test ./... -v'`
Expected: PASS (all tests, including the two new ones and every pre-existing
handler that touches `User`/`Config` — there were none before this plan, so
this is also the first green baseline for the project).

- [ ] **Step 5: No commit** — this directory has no git repo (see Global
  Constraints). Move to the next task; the whole portal is deployed as one
  unit at the end of Task 4.

---

### Task 2: Portal — bearer auth + `POST /api/v1/login`

**Files:**
- Create: `/home/lev/vkturn-ios-portal/api.go`
- Create: `/home/lev/vkturn-ios-portal/api_test.go`

**Interfaces:**
- Consumes: `signSession(cfg *Config, username string, exp int64) string`,
  `verifySession(cfg *Config, token string) (string, bool)`,
  `loadUsers(path string) ([]User, error)`, `findUser(users []User, username string) *User`
  (all from `main.go`, unchanged).
- Produces: `bearerUser(cfg *Config, r *http.Request) string`,
  `handleAPILogin(cfg *Config) http.HandlerFunc`.

- [ ] **Step 1: Write the failing test**

```go
// api_test.go
package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/crypto/bcrypt"
)

func testConfig(t *testing.T, usersFile string) *Config {
	t.Helper()
	return &Config{
		Listen: "x", CertFile: "x", KeyFile: "x",
		CookieSecret: "00112233445566778899aabbccddeeff00112233445566778899aabbccddee",
		ObfKeyHex: "abc", PeerAddress: "1.2.3.4:56000",
		WGIface: "wgcl", WGServerPubkey: "pub", WGConfPath: "/tmp/wg.conf",
		WGSubnetPrefix: "10.13.13.", PoolDir: "/tmp/pool", UsersFile: usersFile,
		HubHost: "1.2.3.4", HubToken: "tok123", HubPin: "pin123",
	}
}

func seedUser(t *testing.T, path, username, password string, extra User) {
	t.Helper()
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		t.Fatal(err)
	}
	extra.Username = username
	extra.PasswordHash = string(hash)
	if err := saveUsers(path, []User{extra}); err != nil {
		t.Fatal(err)
	}
}

func TestHandleAPILoginBadPassword(t *testing.T) {
	dir := t.TempDir()
	usersFile := filepath.Join(dir, "users.json")
	seedUser(t, usersFile, "vasya", "correct-horse", User{AccountID: "acct1"})
	cfg := testConfig(t, usersFile)

	body := strings.NewReader(`{"username":"vasya","password":"wrong"}`)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/login", body)
	w := httptest.NewRecorder()
	handleAPILogin(cfg)(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", w.Code)
	}
}

func TestHandleAPILoginSuccessThenConfig(t *testing.T) {
	dir := t.TempDir()
	usersFile := filepath.Join(dir, "users.json")
	seedUser(t, usersFile, "vasya", "correct-horse", User{
		AccountID: "acct1", DesktopAccounts: []int{8445}, DesktopStreams: 8,
		SplitMode: "exclude", XraySubID: "sub-xyz",
	})
	cfg := testConfig(t, usersFile)

	body := strings.NewReader(`{"username":"vasya","password":"correct-horse"}`)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/login", body)
	w := httptest.NewRecorder()
	handleAPILogin(cfg)(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("bad json: %v", err)
	}
	if resp.Token == "" {
		t.Fatal("empty token")
	}

	// bearerUser must accept what handleAPILogin issued.
	authReq := httptest.NewRequest(http.MethodGet, "/api/v1/config", nil)
	authReq.Header.Set("Authorization", "Bearer "+resp.Token)
	if got := bearerUser(cfg, authReq); got != "vasya" {
		t.Fatalf("bearerUser: expected vasya, got %q", got)
	}
}

func TestBearerUserRejectsMissingOrBadHeader(t *testing.T) {
	cfg := testConfig(t, filepath.Join(t.TempDir(), "users.json"))
	noAuth := httptest.NewRequest(http.MethodGet, "/api/v1/config", nil)
	if got := bearerUser(cfg, noAuth); got != "" {
		t.Fatalf("expected empty for missing header, got %q", got)
	}
	badAuth := httptest.NewRequest(http.MethodGet, "/api/v1/config", nil)
	badAuth.Header.Set("Authorization", "Bearer not-a-real-token")
	if got := bearerUser(cfg, badAuth); got != "" {
		t.Fatalf("expected empty for bad token, got %q", got)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `docker run --rm -u 1000:1001 -e HOME=/tmp -v /home/lev/vkturn-ios-portal:/src -w /src -v /home/lev/go:/go -e GOFLAGS=-buildvcs=false -e GOPROXY=direct golang:latest sh -c 'go test ./... -run TestHandleAPILogin -v'`
Expected: FAIL (compile error — `handleAPILogin`/`bearerUser` undefined).

- [ ] **Step 3: Implement**

```go
// api.go
package main

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"golang.org/x/crypto/bcrypt"
)

// bearerUser mirrors currentUser but reads the "Authorization: Bearer <token>"
// header instead of the "session" cookie — same signSession/verifySession
// mechanism, different transport, so a CLI client that never sees cookies
// can still use the exact same token issuance/verification path as the web UI.
func bearerUser(cfg *Config, r *http.Request) string {
	auth := r.Header.Get("Authorization")
	const prefix = "Bearer "
	if !strings.HasPrefix(auth, prefix) {
		return ""
	}
	username, ok := verifySession(cfg, strings.TrimPrefix(auth, prefix))
	if !ok {
		return ""
	}
	return username
}

func handleAPILogin(cfg *Config) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Username string `json:"username"`
			Password string `json:"password"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}

		usersMu.Lock()
		users, _ := loadUsers(cfg.UsersFile)
		usersMu.Unlock()
		u := findUser(users, req.Username)
		if u == nil || bcrypt.CompareHashAndPassword([]byte(u.PasswordHash), []byte(req.Password)) != nil {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}

		exp := time.Now().Add(30 * 24 * time.Hour).Unix()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(struct {
			Token     string `json:"token"`
			ExpiresAt int64  `json:"expires_at"`
		}{Token: signSession(cfg, req.Username, exp), ExpiresAt: exp})
	}
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `docker run --rm -u 1000:1001 -e HOME=/tmp -v /home/lev/vkturn-ios-portal:/src -w /src -v /home/lev/go:/go -e GOFLAGS=-buildvcs=false -e GOPROXY=direct golang:latest sh -c 'go test ./... -v'`
Expected: PASS.

- [ ] **Step 5: No commit** (no git repo here, per Global Constraints).

---

### Task 3: Portal — `GET /api/v1/config`

**Files:**
- Modify: `/home/lev/vkturn-ios-portal/api.go`
- Modify: `/home/lev/vkturn-ios-portal/api_test.go`

**Interfaces:**
- Consumes: `bearerUser` (Task 2), `loadPoolCreds` (unused here — desktop uses
  live hub creds, not pre-fetched pool snapshots; do not call it), `User`/
  `Config` (Task 1).
- Produces: `DesktopConfig` (JSON shape consumed verbatim by `cmd/desktop`'s
  `apiclient.go` in Task 6 — field names and types must match exactly),
  `handleAPIConfig(cfg *Config) http.HandlerFunc`.

- [ ] **Step 1: Write the failing test**

```go
// append to api_test.go

func TestHandleAPIConfig(t *testing.T) {
	dir := t.TempDir()
	usersFile := filepath.Join(dir, "users.json")
	seedUser(t, usersFile, "vasya", "correct-horse", User{
		AccountID: "acct1", DesktopAccounts: []int{8445, 8446}, DesktopStreams: 8,
		SplitMode: "exclude", XraySubID: "sub-xyz",
	})
	cfg := testConfig(t, usersFile)

	loginReq := httptest.NewRequest(http.MethodPost, "/api/v1/login",
		strings.NewReader(`{"username":"vasya","password":"correct-horse"}`))
	loginW := httptest.NewRecorder()
	handleAPILogin(cfg)(loginW, loginReq)
	var loginResp struct{ Token string `json:"token"` }
	_ = json.Unmarshal(loginW.Body.Bytes(), &loginResp)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/config", nil)
	req.Header.Set("Authorization", "Bearer "+loginResp.Token)
	w := httptest.NewRecorder()
	handleAPIConfig(cfg)(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var got DesktopConfig
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("bad json: %v", err)
	}
	wantURLs := []string{"https://1.2.3.4:8445/turn-creds", "https://1.2.3.4:8446/turn-creds"}
	if len(got.HubURLs) != 2 || got.HubURLs[0] != wantURLs[0] || got.HubURLs[1] != wantURLs[1] {
		t.Fatalf("hubUrls mismatch: %v", got.HubURLs)
	}
	if got.HubToken != "tok123" || got.HubPin != "pin123" || got.Peer != "1.2.3.4:56000" {
		t.Fatalf("hub fields mismatch: %+v", got)
	}
	if got.Streams != 8 || got.SplitMode != "exclude" {
		t.Fatalf("profile fields mismatch: %+v", got)
	}
	if !strings.Contains(got.XraySubscriptionURL, "sub-xyz") {
		t.Fatalf("expected subscription url to contain subId, got %q", got.XraySubscriptionURL)
	}
}

func TestHandleAPIConfigUnauthorized(t *testing.T) {
	cfg := testConfig(t, filepath.Join(t.TempDir(), "users.json"))
	req := httptest.NewRequest(http.MethodGet, "/api/v1/config", nil)
	w := httptest.NewRecorder()
	handleAPIConfig(cfg)(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", w.Code)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `docker run --rm -u 1000:1001 -e HOME=/tmp -v /home/lev/vkturn-ios-portal:/src -w /src -v /home/lev/go:/go -e GOFLAGS=-buildvcs=false -e GOPROXY=direct golang:latest sh -c 'go test ./... -run TestHandleAPIConfig -v'`
Expected: FAIL (compile error — `DesktopConfig`/`handleAPIConfig` undefined).

- [ ] **Step 3: Implement**

Add `"fmt"` to `api.go`'s existing import block from Task 2 (it currently has
`encoding/json`, `net/http`, `strings`, `time`, `golang.org/x/crypto/bcrypt` —
`fmt` is new, needed for `fmt.Sprintf` below):
```go
// append to api.go, below the existing handleAPILogin

// DesktopConfig is the exact JSON shape cmd/desktop's apiclient.go parses.
// Field names/types here and there must stay identical — see Task 6.
type DesktopConfig struct {
	HubURLs             []string `json:"hubUrls"`
	HubPin              string   `json:"hubPin"`
	HubToken            string   `json:"hubToken"`
	Peer                string   `json:"peer"`
	ObfProfile          string   `json:"obfProfile"`
	ObfKey              string   `json:"obfKey"`
	Streams             int      `json:"streams"`
	SplitMode           string   `json:"splitMode"`
	XraySubscriptionURL string   `json:"xraySubscriptionUrl"`
}

func handleAPIConfig(cfg *Config) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		username := bearerUser(cfg, r)
		if username == "" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		usersMu.Lock()
		users, _ := loadUsers(cfg.UsersFile)
		usersMu.Unlock()
		u := findUser(users, username)
		if u == nil {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}

		urls := make([]string, 0, len(u.DesktopAccounts))
		for _, port := range u.DesktopAccounts {
			urls = append(urls, fmt.Sprintf("https://%s:%d/turn-creds", cfg.HubHost, port))
		}
		streams := u.DesktopStreams
		if streams == 0 {
			streams = 10
		}
		out := DesktopConfig{
			HubURLs: urls, HubPin: cfg.HubPin, HubToken: cfg.HubToken,
			Peer: cfg.PeerAddress, ObfProfile: "rtpopus3", ObfKey: cfg.ObfKeyHex,
			Streams: streams, SplitMode: u.SplitMode,
		}
		if u.XraySubID != "" {
			out.XraySubscriptionURL = fmt.Sprintf("https://%s:2096/sub/%s", cfg.HubHost, u.XraySubID)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(out)
	}
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `docker run --rm -u 1000:1001 -e HOME=/tmp -v /home/lev/vkturn-ios-portal:/src -w /src -v /home/lev/go:/go -e GOFLAGS=-buildvcs=false -e GOPROXY=direct golang:latest sh -c 'go test ./... -v'`
Expected: PASS.

- [ ] **Step 5: No commit** (no git repo here).

---

### Task 4: Portal — `cliAddDesktopUser`, route wiring, deploy, live smoke test

**Files:**
- Modify: `/home/lev/vkturn-ios-portal/main.go` (`main()` — add routes and
  `adddesktop` CLI subcommand)

**Interfaces:**
- Produces: `cliAddDesktopUser(cfg *Config, username, accountsCSV string, streams int, splitMode, xraySubID string) error`
  — consumed by panel.js's SSH call in Task 5 (exact positional arg order
  matters there).

- [ ] **Step 1: Implement `cliAddDesktopUser`** (no unit test — this is a thin
  CLI wrapper around already-tested `loadUsers`/`saveUsers`/`bcrypt`, in the
  same style as the untested `cliAddUser` it sits beside; covered instead by
  the live smoke test in Step 5)

```go
// add to main.go, after cliAddUser

func cliAddDesktopUser(cfg *Config, username, accountsCSV string, streams int, splitMode, xraySubID string) error {
	usersMu.Lock()
	defer usersMu.Unlock()
	users, err := loadUsers(cfg.UsersFile)
	if err != nil {
		return err
	}
	if findUser(users, username) != nil {
		return fmt.Errorf("user %q already exists", username)
	}
	var accounts []int
	for _, s := range strings.Split(accountsCSV, ",") {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		port, err := strconv.Atoi(s)
		if err != nil {
			return fmt.Errorf("bad account port %q: %w", s, err)
		}
		accounts = append(accounts, port)
	}
	pwBytes := make([]byte, 12)
	if _, err := rand.Read(pwBytes); err != nil {
		return err
	}
	password := base64.RawURLEncoding.EncodeToString(pwBytes)
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return err
	}
	users = append(users, User{
		Username: username, PasswordHash: string(hash),
		DesktopAccounts: accounts, DesktopStreams: streams,
		SplitMode: splitMode, XraySubID: xraySubID,
	})
	if err := saveUsers(cfg.UsersFile, users); err != nil {
		return err
	}
	fmt.Printf("Added desktop user.\n  username: %s\n  password: %s\n  accounts: %v\n",
		username, password, accounts)
	return nil
}
```

Add `strconv` to the existing import block if not already present (it is not —
check `main.go`'s import list; `strconv` is currently only used indirectly, add
it explicitly if the compiler flags it missing).

- [ ] **Step 2: Wire the CLI subcommand and HTTP routes**

In `main()`, after the existing `deluser` block:
```go
	if len(os.Args) > 1 && os.Args[1] == "adddesktop" {
		if len(os.Args) != 7 {
			log.Fatal("usage: ios-portal adddesktop <username> <account-ports-csv> <streams> <split-mode> <xray-sub-id>")
		}
		streams, err := strconv.Atoi(os.Args[4])
		if err != nil {
			log.Fatalf("adddesktop: bad streams %q: %v", os.Args[4], err)
		}
		if err := cliAddDesktopUser(cfg, os.Args[2], os.Args[3], streams, os.Args[5], os.Args[6]); err != nil {
			log.Fatalf("adddesktop: %v", err)
		}
		return
	}
```
(`os.Args` = `[bin, "adddesktop", username, accountsCSV, streams, splitMode, xraySubID]` = 7 elements.)

Add routes before `log.Printf("vkturn-ios-portal listening...")`:
```go
	mux.HandleFunc("POST /api/v1/login", handleAPILogin(cfg))
	mux.HandleFunc("GET /api/v1/config", handleAPIConfig(cfg))
```

- [ ] **Step 3: Build and run full test suite**

Run: `docker run --rm -u 1000:1001 -e HOME=/tmp -v /home/lev/vkturn-ios-portal:/src -w /src -v /home/lev/go:/go -e GOFLAGS=-buildvcs=false -e GOPROXY=direct golang:latest sh -c 'gofmt -l . && go build ./... && go test ./... -v'`
Expected: `gofmt -l .` prints nothing, build succeeds, all tests PASS.

- [ ] **Step 4: Deploy to VPS-NL**

```bash
docker run --rm -u 1000:1001 -e HOME=/tmp -v /home/lev/vkturn-ios-portal:/src -w /src -v /home/lev/go:/go -e GOFLAGS=-buildvcs=false -e GOPROXY=direct -e GOOS=linux -e GOARCH=amd64 -e CGO_ENABLED=0 golang:latest go build -o ios-portal.bin .
scp /home/lev/vkturn-ios-portal/ios-portal.bin vps:/tmp/vkturn-ios-portal.new
ssh vps 'sudo install -m 755 /tmp/vkturn-ios-portal.new /usr/local/bin/vkturn-ios-portal && sudo systemctl restart vkturn-ios-portal && sleep 1 && systemctl is-active vkturn-ios-portal'
```
Expected final line: `active`.

Then add the two new fields to the **live** config (manual, one-time — do not
script secret injection):
```bash
ssh vps 'sudo -e /etc/vkturn/ios-portal/config.json'
```
Add `"hub_host"`, `"hub_token"`, `"hub_pin"` using the same values already in
`panel-secrets.json` (`S.hubHost`/`S.hubToken`/`S.hubPin` on ai-server — fetch
read-only, do not paste them into any file this plan or its git history
touches):
```bash
ssh ai-lan "node -e \"const s=JSON.parse(require('fs').readFileSync('/home/vkturn/vkturn-panel/panel-secrets.json'));console.log(s.hubHost, s.hubToken, s.hubPin)\""
```
Then `sudo systemctl restart vkturn-ios-portal` again after editing the config.

- [ ] **Step 5: Live smoke test with a disposable user (same pattern as P5)**

```bash
ssh vps '/usr/local/bin/vkturn-ios-portal adddesktop portaltest3 8445 8 exclude ""'
```
Note the printed password. Then:
```bash
TOKEN=$(curl -sk -X POST https://127.0.0.1:8449/api/v1/login \
  -d '{"username":"portaltest3","password":"<paste-printed-password>"}' | \
  python3 -c 'import json,sys;print(json.load(sys.stdin)["token"])')
# run the curl above via: ssh vps '...' — port 8449 is loopback-only on the VPS itself
curl -sk https://127.0.0.1:8449/api/v1/config -H "Authorization: Bearer $TOKEN"
```
Expected: JSON with `hubUrls: ["https://<hub_host>:8445/turn-creds"]`,
`streams: 8`, `splitMode: "exclude"`, correct `hubToken`/`hubPin`.

Clean up:
```bash
ssh vps '/usr/local/bin/vkturn-ios-portal deluser portaltest3'
```

- [ ] **Step 6: No commit** (no git repo here — this task's deliverable is the
  live-verified deployed binary, confirmed in Step 5).

---

### Task 5: `panel.js` — `device=desktop` provisioning

**Files:**
- Modify: `/home/lev/vkturn-android-kit/panel.js` (on `ai-server`, reach via
  `ssh ai-lan`)

**Interfaces:**
- Consumes: `cliAddDesktopUser` CLI shape from Task 4
  (`adddesktop <username> <accounts-csv> <streams> <split-mode> <xray-sub-id>`).
- Produces: a `device=desktop` option in the profile-creation `<select>`,
  parallel to `device=ios`.

- [ ] **Step 1: Add the option and branch in `genArtifact`**

In the `page()` function's `<select name=device>` (mirrors the existing
`<option value=ios>iPhone</option>`):
```html
<option value=desktop>Windows/Linux (login)</option>
```

Add a `genDesktopArtifact` function next to the existing `genIosArtifact`
(`/home/lev/vkturn-android-kit/panel.js`, same file, same pattern — SSH call
instead of local file generation):
```javascript
// Desktop-профиль не генерит файл локально - заводит логин на портале так же,
// как genIosArtifact, только другим CLI-подкомандой с доп. полями (аккаунты,
// streams, split, xray subId). Портал сам строит /api/v1/config на лету.
function genDesktopArtifact(prof) {
  const ssh = S.wg && S.wg.serverSsh; if (!ssh) throw new Error('wg.serverSsh не задан');
  const port = String((S.wg && S.wg.serverSshPort) || 22);
  const username = slugify(prof.name);
  const accountsCsv = prof.accounts.join(',');
  const streams = prof.streams || 10;
  const splitMode = prof.splitMode || 'exclude';
  const xraySubId = prof.xraySubId || '';
  let out;
  try {
    out = execFileSync('ssh', ['-p', port, '-o', 'BatchMode=yes', '-o', 'ConnectTimeout=10', ssh,
      'vkturn-ios-portal', 'adddesktop', username, accountsCsv, String(streams), splitMode, xraySubId],
      { timeout: 20000 }).toString();
  } catch (e) {
    throw new Error('adddesktop не прошёл: ' + ((e.stderr && e.stderr.toString().trim()) || e.message));
  }
  const password = (out.match(/password:\s*(\S+)/) || [])[1];
  if (!password) throw new Error('не разобрал пароль из вывода adddesktop:\n' + out);
  const text = [
    `VK-TURN — доступ для Windows/Linux (${prof.name})`, '',
    '1. Скачай vkturn-desktop с релиза (см. README репозитория).',
    '2. Запусти в терминале, введи логин/пароль при первом запуске:',
    `   Логин: ${username}`, `   Пароль: ${password}`,
    '3. Стрелками выбери режим (vk-turn / xray-подписка), Enter — подключиться.',
  ].join('\n') + '\n';
  const file = path.join(OUTBOX, `${prof.id}.txt`);
  fs.writeFileSync(file, text);
  return { file, filename: `vkturn-desktop-${username}.txt`, mime: 'text/plain',
           desktopUsername: username,
           note: `desktop-портал, логин ${username}, аккаунты ${accountsCsv}` };
}
```

Wire it into `genArtifact`'s dispatch (next to the existing
`if (prof.device === 'ios') return genIosArtifact(prof);`):
```javascript
  if (prof.device === 'desktop') return genDesktopArtifact(prof);
```

- [ ] **Step 2: Persist `desktopUsername` on the profile (mirrors how
  `iosUsername` is already persisted) and wire deletion**

In the `POST /create` handler, find the existing line
`if (art.iosUsername) prof.iosUsername = art.iosUsername;` and add directly
after it:
```javascript
      if (art.desktopUsername) prof.desktopUsername = art.desktopUsername;
```

In the `POST /delete/:id` handler, find the existing block:
```javascript
      if (gone && gone.iosUsername) {                                    // авто-снятие логина+пира (ios)
        const ssh = S.wg && S.wg.serverSsh;
        try {
          execFileSync('ssh', ['-o', 'BatchMode=yes', '-o', 'ConnectTimeout=10', ssh,
            'vkturn-ios-portal', 'deluser', gone.iosUsername], { timeout: 20000 });
        } catch (e) {
          console.error('deluser failed for', gone.iosUsername, ':', (e.stderr && e.stderr.toString().trim()) || e.message);
        }
      }
```
and add a parallel block directly after it:
```javascript
      if (gone && gone.desktopUsername) {                                // авто-снятие логина (desktop)
        const ssh = S.wg && S.wg.serverSsh;
        try {
          execFileSync('ssh', ['-o', 'BatchMode=yes', '-o', 'ConnectTimeout=10', ssh,
            'vkturn-ios-portal', 'deluser', gone.desktopUsername], { timeout: 20000 });
        } catch (e) {
          console.error('deluser failed for', gone.desktopUsername, ':', (e.stderr && e.stderr.toString().trim()) || e.message);
        }
      }
```
(`deluser` is the existing, unmodified subcommand from `main.go` — it already
removes any `User` by username regardless of whether they were added via
`adduser` or `adddesktop`, since both write into the same `users.json`. No
portal-side change needed for deletion, only this panel.js call site.)

- [ ] **Step 3: Live smoke test with a disposable profile (same pattern as P5)**

Via the panel UI (`ssh -L 8088:127.0.0.1:8088 -N ai-lan`, browse
`http://127.0.0.1:8088`) or curl:
```bash
ssh ai-lan "curl -s -o /dev/null -w '%{http_code}\n' -X POST \
  --data-urlencode 'name=paneltest-desktop' --data-urlencode 'device=desktop' \
  --data-urlencode 'streams=8' --data-urlencode 'account=8445' \
  --data-urlencode 'splitMode=exclude' http://127.0.0.1:8088/create"
```
Expected: `302`. Then confirm the profile's `note` field mentions
`desktop-портал` (same inspection pattern as P5's `node -e` snippet against
`panel-profiles.json`), and that
`ssh vps '/usr/local/bin/vkturn-ios-portal adddesktop ...'` really ran
(check `/etc/vkturn/ios-portal/users.json` on the VPS gained the new user).

Delete the test profile via `/delete/:id` afterward (as in P5) and confirm the
portal user is also gone (`ssh vps 'cat /etc/vkturn/ios-portal/users.json'`
no longer lists the test username — exercises the Step 2 delete branch).

- [ ] **Step 4: No commit** (panel.js has no git repo either, per prior
  sessions — confirm with `ssh ai-lan "cd /home/vkturn/vkturn-panel && git status"`
  before assuming; if it turns out to be a git repo, commit normally with a
  message describing the new device type).

---

### Task 6: `cmd/desktop` — API client (login + config cache)

**Files:**
- Create: `cmd/desktop/config.go`
- Create: `cmd/desktop/apiclient.go`
- Test: `cmd/desktop/apiclient_test.go`

**Interfaces:**
- Produces: `DesktopConfig` struct (must match Task 3's JSON shape exactly),
  `Login(ctx context.Context, baseURL, username, password string) (token string, err error)`,
  `FetchConfig(ctx context.Context, baseURL, token string) (*DesktopConfig, error)`,
  `CachePath() (string, error)`, `SaveCache(cfg *DesktopConfig) error`,
  `LoadCache() (*DesktopConfig, error)`.

- [ ] **Step 1: Write the failing test**

```go
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
```

- [ ] **Step 2: Run test to verify it fails**

Run: `docker run --rm -u 1000:1001 -e HOME=/tmp -v /home/lev/free-turn-proxy-levnasru/free-turn-proxy/.claude/worktrees/desktop-client-milestone-a:/src -w /src -v /home/lev/go:/go -e GOFLAGS=-buildvcs=false -e GOPROXY=direct golang:latest sh -c 'go test ./cmd/desktop/... -v'`
Expected: FAIL (package doesn't exist yet / undefined symbols).

- [ ] **Step 3: Implement**

```go
// cmd/desktop/config.go
package main

// DesktopConfig mirrors vkturn-ios-portal's api.go DesktopConfig byte-for-byte
// (JSON field names must match — two separate Go modules, no shared package).
type DesktopConfig struct {
	HubURLs             []string `json:"hubUrls"`
	HubPin              string   `json:"hubPin"`
	HubToken            string   `json:"hubToken"`
	Peer                string   `json:"peer"`
	ObfProfile          string   `json:"obfProfile"`
	ObfKey              string   `json:"obfKey"`
	Streams             int      `json:"streams"`
	SplitMode           string   `json:"splitMode"`
	XraySubscriptionURL string   `json:"xraySubscriptionUrl"`
}
```

```go
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
)

func Login(ctx context.Context, baseURL, username, password string) (string, error) {
	body, _ := json.Marshal(map[string]string{"username": username, "password": password})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+"/api/v1/login", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
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

func FetchConfig(ctx context.Context, baseURL, token string) (*DesktopConfig, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/api/v1/config", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
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
	return os.WriteFile(path, data, 0o600)
}

func LoadCache() (*DesktopConfig, error) {
	path, err := CachePath()
	if err != nil {
		return nil, err
	}
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

```

- [ ] **Step 4: Run test to verify it passes**

Run (docker wrapper, see Global Constraints — substitute your actual worktree
path from `pwd`): `docker run --rm -u 1000:1001 -e HOME=/tmp -v /home/lev/free-turn-proxy-levnasru/free-turn-proxy/.claude/worktrees/desktop-client-milestone-a:/src -w /src -v /home/lev/go:/go -e GOFLAGS=-buildvcs=false -e GOPROXY=direct golang:latest sh -c 'gofmt -l cmd/desktop && go build ./cmd/desktop/... && go test ./cmd/desktop/... -v'`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add cmd/desktop/config.go cmd/desktop/apiclient.go cmd/desktop/apiclient_test.go
git commit -m "feat(desktop): add portal login + config fetch/cache client"
```

---

### Task 7: `cmd/desktop` — arrow-key terminal menu

**Files:**
- Create: `cmd/desktop/menu.go`
- Test: `cmd/desktop/menu_test.go`

**Interfaces:**
- Produces: `selectFromKeys(items []string, keys io.Reader, out io.Writer) (int, error)`
  (pure, testable), `RunMenu(items []string) (int, error)` (thin raw-mode
  wrapper around stdin/stdout, not unit-tested — exercised by the final live
  run in Task 9).

- [ ] **Step 1: Write the failing test**

```go
// cmd/desktop/menu_test.go
package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestSelectFromKeysArrowsAndEnter(t *testing.T) {
	items := []string{"vk-turn", "xray-подписка", "выход"}
	// down, down, up, enter -> index 1
	keys := strings.NewReader("\x1b[B\x1b[B\x1b[A\r")
	var out bytes.Buffer
	idx, err := selectFromKeys(items, keys, &out)
	if err != nil {
		t.Fatalf("selectFromKeys: %v", err)
	}
	if idx != 1 {
		t.Fatalf("expected index 1, got %d", idx)
	}
	if !strings.Contains(out.String(), "xray-подписка") {
		t.Fatalf("expected rendered output to mention selection, got %q", out.String())
	}
}

func TestSelectFromKeysWrapsAtBoundaries(t *testing.T) {
	items := []string{"a", "b"}
	// up from index 0 wraps to last item, then enter
	keys := strings.NewReader("\x1b[A\r")
	idx, err := selectFromKeys(items, keys, &bytes.Buffer{})
	if err != nil {
		t.Fatal(err)
	}
	if idx != 1 {
		t.Fatalf("expected wrap to index 1, got %d", idx)
	}
}

func TestSelectFromKeysCtrlCReturnsError(t *testing.T) {
	items := []string{"a", "b"}
	keys := strings.NewReader("\x03")
	_, err := selectFromKeys(items, keys, &bytes.Buffer{})
	if err == nil {
		t.Fatal("expected error on Ctrl+C")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `docker run --rm -u 1000:1001 -e HOME=/tmp -v /home/lev/free-turn-proxy-levnasru/free-turn-proxy/.claude/worktrees/desktop-client-milestone-a:/src -w /src -v /home/lev/go:/go -e GOFLAGS=-buildvcs=false -e GOPROXY=direct golang:latest sh -c 'go test ./cmd/desktop/... -run TestSelectFromKeys -v'`
Expected: FAIL (undefined `selectFromKeys`).

- [ ] **Step 3: Implement**

```go
// cmd/desktop/menu.go
package main

import (
	"errors"
	"fmt"
	"io"
	"os"

	"golang.org/x/term"
)

// selectFromKeys reads raw key bytes from `keys` (already in whatever mode
// the caller set up — this function does no terminal-mode switching itself,
// which is what makes it testable with a plain strings.Reader) and returns
// the selected index once Enter/'\r'/'\n' arrives. Ctrl+C ('\x03') returns
// an error so the caller can exit cleanly instead of looping forever.
func selectFromKeys(items []string, keys io.Reader, out io.Writer) (int, error) {
	sel := 0
	render := func() {
		fmt.Fprint(out, "\r\n")
		for i, it := range items {
			marker := "  "
			if i == sel {
				marker = "> "
			}
			fmt.Fprintf(out, "%s%s\r\n", marker, it)
		}
	}
	render()

	buf := make([]byte, 3)
	for {
		n, err := keys.Read(buf)
		if err != nil {
			if errors.Is(err, io.EOF) {
				return 0, errors.New("selectFromKeys: input closed before selection")
			}
			return 0, err
		}
		chunk := buf[:n]
		switch {
		case n == 1 && chunk[0] == 0x03:
			return 0, errors.New("selectFromKeys: interrupted")
		case n == 1 && (chunk[0] == '\r' || chunk[0] == '\n'):
			return sel, nil
		case n == 3 && chunk[0] == 0x1b && chunk[1] == '[' && chunk[2] == 'A': // up
			sel = (sel - 1 + len(items)) % len(items)
			render()
		case n == 3 && chunk[0] == 0x1b && chunk[1] == '[' && chunk[2] == 'B': // down
			sel = (sel + 1) % len(items)
			render()
		}
	}
}

// RunMenu puts stdin into raw mode, runs selectFromKeys against it, restores
// the terminal, and returns the selected item text (not just its index — the
// caller in main.go switches on the string).
func RunMenu(items []string) (string, error) {
	fd := int(os.Stdin.Fd())
	oldState, err := term.MakeRaw(fd)
	if err != nil {
		return "", fmt.Errorf("raw mode: %w", err)
	}
	defer term.Restore(fd, oldState)

	idx, err := selectFromKeys(items, os.Stdin, os.Stdout)
	if err != nil {
		return "", err
	}
	return items[idx], nil
}
```

- [ ] **Step 4: Add `golang.org/x/term` dependency and run test**

```bash
docker run --rm -u 1000:1001 -e HOME=/tmp -v /home/lev/free-turn-proxy-levnasru/free-turn-proxy/.claude/worktrees/desktop-client-milestone-a:/src -w /src -v /home/lev/go:/go -e GOFLAGS=-buildvcs=false -e GOPROXY=direct golang:latest sh -c 'go get golang.org/x/term && gofmt -l cmd/desktop && go build ./cmd/desktop/... && go test ./cmd/desktop/... -v'
```
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add cmd/desktop/menu.go cmd/desktop/menu_test.go go.mod go.sum
git commit -m "feat(desktop): add arrow-key terminal menu"
```

---

### Task 8: `cmd/desktop` — mode 1 launcher (vk-turn subprocess)

**Files:**
- Create: `cmd/desktop/launcher.go`
- Test: `cmd/desktop/launcher_test.go`

**Interfaces:**
- Consumes: `DesktopConfig` (Task 6).
- Produces: `buildClientArgs(cfg *DesktopConfig) (args, env []string)`,
  `RunClient(ctx context.Context, clientBinPath string, cfg *DesktopConfig, stdout, stderr io.Writer) error`.

- [ ] **Step 1: Write the failing test**

```go
// cmd/desktop/launcher_test.go
package main

import (
	"reflect"
	"testing"
)

func TestBuildClientArgs(t *testing.T) {
	cfg := &DesktopConfig{
		HubURLs: []string{"https://x:8445/turn-creds"}, HubPin: "pin1", HubToken: "tok1",
		Peer: "1.2.3.4:56000", ObfProfile: "rtpopus3", ObfKey: "key1", Streams: 8,
	}
	args, env := buildClientArgs(cfg)

	want := []string{
		"-provider", "hub", "-hub-url", "https://x:8445/turn-creds", "-hub-pin", "pin1",
		"-peer", "1.2.3.4:56000", "-mode", "tcp", "-bond",
		"-obf-profile", "rtpopus3", "-obf-key", "key1", "-n", "8",
		"-listen", "127.0.0.1:9000",
	}
	if !reflect.DeepEqual(args, want) {
		t.Fatalf("args mismatch:\n got  %v\n want %v", args, want)
	}

	found := false
	for _, e := range env {
		if e == "VKTURN_HUB_TOKEN=tok1" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected VKTURN_HUB_TOKEN in env, got %v", env)
	}
}

func TestBuildClientArgsMultipleHubURLs(t *testing.T) {
	cfg := &DesktopConfig{
		HubURLs: []string{"https://x:8445/turn-creds", "https://x:8446/turn-creds"},
		HubPin: "pin1", HubToken: "tok1", Peer: "1.2.3.4:56000",
		ObfProfile: "rtpopus3", ObfKey: "key1", Streams: 8,
	}
	args, _ := buildClientArgs(cfg)
	joined := ""
	for i, a := range args {
		if a == "-hub-url" {
			joined = args[i+1]
		}
	}
	if joined != "https://x:8445/turn-creds,https://x:8446/turn-creds" {
		t.Fatalf("expected comma-joined hub-url, got %q", joined)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `docker run --rm -u 1000:1001 -e HOME=/tmp -v /home/lev/free-turn-proxy-levnasru/free-turn-proxy/.claude/worktrees/desktop-client-milestone-a:/src -w /src -v /home/lev/go:/go -e GOFLAGS=-buildvcs=false -e GOPROXY=direct golang:latest sh -c 'go test ./cmd/desktop/... -run TestBuildClientArgs -v'`
Expected: FAIL (undefined `buildClientArgs`).

- [ ] **Step 3: Implement**

```go
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
```

- [ ] **Step 4: Run test to verify it passes**

Run: `docker run --rm -u 1000:1001 -e HOME=/tmp -v /home/lev/free-turn-proxy-levnasru/free-turn-proxy/.claude/worktrees/desktop-client-milestone-a:/src -w /src -v /home/lev/go:/go -e GOFLAGS=-buildvcs=false -e GOPROXY=direct golang:latest sh -c 'gofmt -l cmd/desktop && go build ./cmd/desktop/... && go test ./cmd/desktop/... -v'`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add cmd/desktop/launcher.go cmd/desktop/launcher_test.go
git commit -m "feat(desktop): add vk-turn mode subprocess launcher"
```

---

### Task 9: `cmd/desktop` — mode 2 launcher (xray subscription), `main.go`, goreleaser, final live test

**Files:**
- Modify: `cmd/desktop/launcher.go`
- Modify: `cmd/desktop/launcher_test.go`
- Create: `cmd/desktop/main.go`
- Modify: `.goreleaser.yaml`

**Interfaces:**
- Consumes: everything from Tasks 6-8.
- Produces: `convertSubscription(body string) ([]string, error)` (list of xray
  JSON configs, one per share-link), `RunXray(ctx context.Context, xrayBinPath, xrayJSON string, stdout, stderr io.Writer) error`.

- [ ] **Step 1: Confirm the real `libXray` import path and signature before
  writing code against it** — do not trust a remembered/guessed path.

```bash
docker run --rm -u 1000:1001 -e HOME=/tmp -v /home/lev/free-turn-proxy-levnasru/free-turn-proxy/.claude/worktrees/desktop-client-milestone-a:/src -w /src -v /home/lev/go:/go -e GOFLAGS=-buildvcs=false -e GOPROXY=direct golang:latest sh -c 'go get github.com/xtls/libXray@latest && go doc github.com/xtls/libXray Invoke'
```
Read the actual output. If the resolved module path differs in casing from
`github.com/xtls/libXray` (Go modules are case-sensitive as published), use
whatever `go get` actually recorded in `go.mod` for every import in this task.
If `Invoke` isn't the right symbol name per `go doc`'s output, adjust the code
below accordingly — the request/response JSON shape (`apiVersion`/`method`/
`payload`/`success`/`data`) is confirmed independently from
`turn-proxy-android`'s already-shipped `XraySubscriptionFetcher.kt`, so that
part does not need re-verification, only the exact Go call site does.

- [ ] **Step 2: Write the failing test**

```go
// append to cmd/desktop/launcher_test.go

func TestConvertSubscriptionParsesShareLinks(t *testing.T) {
	// Two share-links, base64-joined by newline, as a real 3x-ui subscription
	// body would look (see docs/sub.md-adjacent research in the design doc).
	body := "dmxlc3M6Ly9leGFtcGxlLTEKdmxlc3M6Ly9leGFtcGxlLTI=" // "vless://example-1\nvless://example-2"
	_, err := convertSubscription(body)
	// This link isn't a real vless URI, so libXray will fail to convert it —
	// that's fine for this test, which only checks that convertSubscription
	// doesn't error on decode/line-splitting itself. Full conversion is
	// exercised against a real subscription URL in the Step 6 live test.
	if err != nil {
		t.Fatalf("convertSubscription should not fail on decode/split, got: %v", err)
	}
}

func TestConvertSubscriptionRejectsEmptyBody(t *testing.T) {
	_, err := convertSubscription("")
	if err == nil {
		t.Fatal("expected error for empty subscription body")
	}
}
```

- [ ] **Step 3: Run test to verify it fails**

Run: `docker run --rm -u 1000:1001 -e HOME=/tmp -v /home/lev/free-turn-proxy-levnasru/free-turn-proxy/.claude/worktrees/desktop-client-milestone-a:/src -w /src -v /home/lev/go:/go -e GOFLAGS=-buildvcs=false -e GOPROXY=direct golang:latest sh -c 'go test ./cmd/desktop/... -run TestConvertSubscription -v'`
Expected: FAIL (undefined `convertSubscription`).

- [ ] **Step 4: Implement** (adjust the `libXray` call per what Step 1 found —
  the shape below assumes `Invoke` matched the confirmed JNI-mirrored API)

Task 8 already created `launcher.go` with an import block of `context`, `io`,
`os/exec`, `strconv`, `strings`. Do not add a second `import (...)` block —
Go rejects importing the same package twice in one file. Instead edit that
existing block in place to read:
```go
import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"runtime" // also needed by Step 5 below — add both now, one edit
	"strconv"
	"strings"

	libxray "github.com/xtls/libXray" // adjust casing/path per Task 9 Step 1's `go doc` output
)
```

Then append the new functions below the existing ones:
```go
// convertSubscription decodes a 3x-ui-style subscription body (base64 of
// newline-separated share-links, or plain-text share-links if the body
// isn't valid base64) and converts each line to an xray-core JSON config via
// libXray — the same conversion turn-proxy-android's XraySubscriptionFetcher.kt
// already does over JNI; here it's a direct Go call, no bridge needed.
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
			Success bool   `json:"success"`
			Data    string `json:"data"`
		}
		if err := json.Unmarshal([]byte(respRaw), &resp); err != nil || !resp.Success || resp.Data == "" {
			continue // skip unparseable lines, same tolerance as the Android fetcher
		}
		configs = append(configs, resp.Data)
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
```

- [ ] **Step 5: Write `main.go`, run full test suite**

```go
// cmd/desktop/main.go
package main

import (
	"bufio"
	"context"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
)

const portalBaseURL = "https://lft.levnas.ru"

func main() {
	cfg, err := LoadCache()
	if err != nil {
		cfg, err = loginFlow()
		if err != nil {
			fmt.Fprintln(os.Stderr, "login failed:", err)
			os.Exit(1)
		}
	}

	for {
		choice, err := RunMenu([]string{"vk-turn", "xray-подписка", "обновить конфиг", "выход"})
		if err != nil {
			fmt.Fprintln(os.Stderr, "\nmenu:", err)
			return
		}
		switch choice {
		case "vk-turn":
			runMode(cfg, "vk-turn")
		case "xray-подписка":
			runMode(cfg, "xray")
		case "обновить конфиг":
			refreshed, err := loginFlow()
			if err != nil {
				fmt.Fprintln(os.Stderr, "refresh failed:", err)
				continue
			}
			cfg = refreshed
		case "выход":
			return
		}
	}
}

func loginFlow() (*DesktopConfig, error) {
	reader := bufio.NewReader(os.Stdin)
	fmt.Print("Логин: ")
	username, _ := reader.ReadString('\n')
	fmt.Print("Пароль: ")
	password, _ := reader.ReadString('\n') // Milestone A: echoed; term.ReadPassword can replace this later
	username = strings.TrimSpace(username)
	password = strings.TrimSpace(password)

	ctx := context.Background()
	token, err := Login(ctx, portalBaseURL, username, password)
	if err != nil {
		return nil, err
	}
	cfg, err := FetchConfig(ctx, portalBaseURL, token)
	if err != nil {
		return nil, err
	}
	if err := SaveCache(cfg); err != nil {
		fmt.Fprintln(os.Stderr, "warning: could not cache config:", err)
	}
	return cfg, nil
}

func runMode(cfg *DesktopConfig, mode string) {
	exeDir, err := os.Executable()
	if err != nil {
		fmt.Fprintln(os.Stderr, "resolve executable path:", err)
		return
	}
	dir := filepath.Dir(exeDir)

	ctx, cancel := context.WithCancel(context.Background())
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() { <-sigCh; cancel() }()
	defer signal.Stop(sigCh)

	switch mode {
	case "vk-turn":
		binPath := filepath.Join(dir, clientBinName())
		if err := RunClient(ctx, binPath, cfg, os.Stdout, os.Stderr); err != nil {
			fmt.Fprintln(os.Stderr, "client exited:", err)
		}
	case "xray":
		if cfg.XraySubscriptionURL == "" {
			fmt.Fprintln(os.Stderr, "нет xray-подписки для этого профиля")
			return
		}
		resp, err := http.Get(cfg.XraySubscriptionURL) //nolint:noctx // short-lived CLI tool, ctx wiring deferred to Milestone B
		if err != nil {
			fmt.Fprintln(os.Stderr, "fetch subscription:", err)
			return
		}
		defer resp.Body.Close()
		body := make([]byte, 0, 4096)
		buf := make([]byte, 4096)
		for {
			n, rerr := resp.Body.Read(buf)
			body = append(body, buf[:n]...)
			if rerr != nil {
				break
			}
		}
		configs, err := convertSubscription(string(body))
		if err != nil || len(configs) == 0 {
			fmt.Fprintln(os.Stderr, "no usable xray configs in subscription:", err)
			return
		}
		binPath := filepath.Join(dir, xrayBinName())
		if err := RunXray(ctx, binPath, configs[0], os.Stdout, os.Stderr); err != nil {
			fmt.Fprintln(os.Stderr, "xray exited:", err)
		}
	}
}
```

`clientBinName`/`xrayBinName` need platform-suffix handling (`.exe` on
Windows) — add to `launcher.go` (`runtime` is already in the merged import
block from Step 4):
```go
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
```

Run (native build+test first, then each cross-compile as its own container —
GOOS differs per invocation, and the cross-compiled binaries need to land in
the host's `/tmp`, not the container's ephemeral one, so these two also mount
`/tmp:/tmp` explicitly):
```bash
docker run --rm -u 1000:1001 -e HOME=/tmp -v /home/lev/free-turn-proxy-levnasru/free-turn-proxy/.claude/worktrees/desktop-client-milestone-a:/src -w /src -v /home/lev/go:/go -e GOFLAGS=-buildvcs=false -e GOPROXY=direct golang:latest sh -c 'gofmt -l cmd/desktop && go build ./cmd/desktop/... && go test ./cmd/desktop/... -v'

docker run --rm -u 1000:1001 -e HOME=/tmp -v /home/lev/free-turn-proxy-levnasru/free-turn-proxy/.claude/worktrees/desktop-client-milestone-a:/src -w /src -v /home/lev/go:/go -v /tmp:/tmp -e GOFLAGS=-buildvcs=false -e GOPROXY=direct -e GOOS=windows -e GOARCH=amd64 -e CGO_ENABLED=0 golang:latest go build -o /tmp/vkturn-desktop.exe ./cmd/desktop

docker run --rm -u 1000:1001 -e HOME=/tmp -v /home/lev/free-turn-proxy-levnasru/free-turn-proxy/.claude/worktrees/desktop-client-milestone-a:/src -w /src -v /home/lev/go:/go -v /tmp:/tmp -e GOFLAGS=-buildvcs=false -e GOPROXY=direct -e GOOS=linux -e GOARCH=amd64 -e CGO_ENABLED=0 golang:latest go build -o /tmp/vkturn-desktop ./cmd/desktop
```
Expected: everything builds clean, all tests PASS, both cross-compiles
succeed, and `ls -la /tmp/vkturn-desktop /tmp/vkturn-desktop.exe` on the host
(outside any container) shows both binaries owned by your normal user, not
root.

- [ ] **Step 6: Add `cmd/desktop` to the release pipeline**

In `.goreleaser.yaml`, add a new build block after the existing `client-android`
build:
```yaml
  - id: desktop
    main: ./cmd/desktop
    binary: vkturn-desktop
    flags:
      - -trimpath
    ldflags:
      - -s -w -X main.version={{.Version}}
    env:
      - CGO_ENABLED=0
    targets:
      - windows_amd64
      - linux_amd64
```
And add `desktop` to the existing `raw` archive's `ids` list (alongside
`client`/`server`):
```yaml
  - id: raw
    ids:
      - client
      - server
      - desktop
```

- [ ] **Step 7: Live end-to-end smoke test with the disposable portal user from
  Task 4/5 (same discipline as P5 — create, verify, tear down)**

Using the `paneltest-desktop` profile created in Task 5 (or a fresh one if it
was already cleaned up):
```bash
scp /tmp/vkturn-desktop vps:/tmp/  # or run locally against lft.levnas.ru — either works, it's a public HTTPS endpoint
# copy the already-published client binary alongside it for the vk-turn mode test:
curl -sLo /tmp/client https://github.com/levnasru/free-turn-proxy/releases/latest/download/client-linux-amd64
chmod +x /tmp/vkturn-desktop /tmp/client
cd /tmp && ./vkturn-desktop
# at the prompt: enter the disposable profile's username/password, arrow to
# "vk-turn", Enter — confirm it connects (log line matching what cmd/client
# normally prints on a successful TURN allocation), then Ctrl+C, confirm
# clean exit (no leftover client process: `ps aux | grep client`).
```
Expected: login succeeds, config fetched (`~/.vkturn/config.json` created,
`0600`), vk-turn mode connects and Ctrl+C stops it cleanly with no orphaned
process.

Clean up the disposable portal user afterward exactly as in Task 4/5's smoke
tests.

- [ ] **Step 8: Commit**

```bash
git add cmd/desktop/launcher.go cmd/desktop/launcher_test.go cmd/desktop/main.go \
        .goreleaser.yaml go.mod go.sum
git commit -m "feat(desktop): add xray-subscription mode, main entrypoint, release build"
```

---

## Self-Review Notes

- **Spec coverage:** Login/config-fetch (Task 2-3, 6), terminal menu no GUI
  (Task 7), vk-turn mode (Task 8), xray-subscription mode (Task 9),
  `panel.js` `device=desktop` provisioning (Task 5), goreleaser build (Task 9
  Step 6) — all Milestone A spec sections have a task. WG full-tunnel
  (Milestone B) is deliberately out of scope for this plan, per the spec's
  phasing.
- **Design correction vs. the spec:** the spec's Mode 1 description said
  vk-turn mode runs "in-process, same code as `cmd/client`." Task 8 instead
  spawns the built `client` binary as a subprocess. Reason found while
  drafting this plan: `cmd/client`'s `main()` isn't factored into a reusable,
  non-`os.Exit`-ing function, and refactoring it would mean editing upstream
  code beyond the fork's three sanctioned patch points. Subprocess is simpler,
  safer (a vk-turn crash can't take down the whole desktop menu process), and
  symmetric with how xray is already spawned in mode 2 and in today's manual
  kits. Functionally equivalent from the user's point of view.
- **Type consistency:** `DesktopConfig` defined identically in Task 3
  (`api.go`) and Task 6 (`cmd/desktop/config.go`) — same JSON tags, same
  field order documented, cross-checked in Task 3's test against Task 6's
  struct shape.
- **No placeholders:** every step has runnable code and concrete expected
  output; the one explicitly deferred detail (Linux system-proxy toggle
  mechanism, mentioned in the spec) intentionally isn't needed for Milestone A
  — xray in this plan is used standalone (SOCKS on `127.0.0.1`, the user
  points other tools at it manually or Milestone B's WG mode supersedes the
  need), not wired into an OS-wide proxy toggle. If OS-wide SOCKS toggling
  turns out to be wanted before Milestone B, that's a small follow-up task,
  not a gap in this plan's own stated scope.
