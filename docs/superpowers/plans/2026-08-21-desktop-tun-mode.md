# Desktop TUN Mode Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add a `vk-turn (tun)` mode to `vkturn-desktop` (Windows + Linux) that captures the whole machine's traffic through the VK-TURN tunnel via xray-core's built-in `tun` inbound, so family members don't have to configure a SOCKS5 proxy by hand. The existing SOCKS5 flow stays available as `vk-turn (socks)`.

**Architecture:** `client` (TURN+bond) is unchanged. `xray` gets a new config variant: `tun` inbound instead of `socks`, with `autoOutboundsInterface` pointing at the real physical uplink (avoids the classic tun routing-loop) and `autoSystemRoutingTable` set to the public-internet complement of a fixed LAN/private CIDR list (mirrors Android's `RealityVpnService` exclusion policy). Creating the TUN interface needs elevated privileges, requested only when the user picks the tun menu item (UAC on Windows via `ShellExecute`+`runas`, `pkexec` on Linux), never for the other menu items.

**Tech Stack:** Go (stdlib only for the new logic — no new dependencies; `xray-core`'s `proxy/tun` and `golang.zx2c4.com/wintun` are already transitive deps, now actually invoked). `golang.org/x/sys/windows` for the Windows elevation check (already indirectly available via `golang.org/x/sys` in `go.sum`; needs adding as a direct import in `go.mod` — `go mod tidy` will do this in Task 3).

**Spec:** `docs/superpowers/specs/2026-08-21-desktop-tun-mode-design.md`

## Global Constraints

- No local Go toolchain in this environment — every `go build`/`go test`/`go vet`/`gofmt` command in this plan runs inside Docker: `docker run --rm -v "$PWD":/src -w /src golang:latest <command>`. Pass `-buildvcs=false` to `go build`/`go test`/`go vet` (the mounted repo trips VCS-stamping otherwise).
- Existing house test style: plain stdlib `testing`, table/literal assertions with `reflect.DeepEqual` where useful — no testify, no test framework. See `cmd/desktop/launcher_test.go` for the pattern to match.
- Platform-specific code goes in `_linux.go`/`_windows.go` files with implicit Go build-tag-by-filename — same convention already used for `cmd/desktop/tray.go`/`tray_other.go`. Do not use `runtime.GOOS` branching inside a single shared file for anything that needs different imports per OS.
- No secrets in committed files. `HubToken` etc. already travel via env/cache file, untouched by this plan.
- Every new exported/package-level function needs a doc comment explaining *why*, not *what*, matching the existing comment style in `cmd/desktop/*.go` (see `waitForListening`'s comment for the target density).
- Commit after each task, not after each step.

---

### Task 1: LAN-exclusion route list

**Files:**
- Create: `cmd/desktop/lanexclude.go`
- Test: `cmd/desktop/lanexclude_test.go`

**Interfaces:**
- Produces: `publicRoutes() []string` — the public-internet CIDR blocks (IPv4 space minus the fixed private/reserved ranges below), used by Task 4's xray config builder as `autoSystemRoutingTable`.

- [ ] **Step 1: Write the failing test**

```go
// cmd/desktop/lanexclude_test.go
package main

import (
	"sort"
	"testing"
)

func TestPublicRoutesExcludePrivateRanges(t *testing.T) {
	routes := publicRoutes()
	if len(routes) == 0 {
		t.Fatal("publicRoutes() returned no routes")
	}

	excluded := []string{
		"192.168.1.1", "10.0.0.5", "172.16.5.5", "169.254.1.1",
		"127.0.0.1", "255.255.255.255", "224.0.0.1",
	}
	for _, ip := range excluded {
		if routeCovers(routes, ip) {
			t.Errorf("publicRoutes() unexpectedly covers private/reserved address %s", ip)
		}
	}

	included := []string{"8.8.8.8", "1.1.1.1", "91.231.135.181"}
	for _, ip := range included {
		if !routeCovers(routes, ip) {
			t.Errorf("publicRoutes() does not cover public address %s", ip)
		}
	}
}

func TestPublicRoutesDoNotOverlap(t *testing.T) {
	routes := publicRoutes()
	var ranges []ipv4Range
	for _, cidr := range routes {
		r, err := cidrToRange(cidr)
		if err != nil {
			t.Fatalf("publicRoutes() produced invalid CIDR %q: %v", cidr, err)
		}
		ranges = append(ranges, r)
	}
	sort.Slice(ranges, func(i, j int) bool { return ranges[i].lo < ranges[j].lo })
	for i := 1; i < len(ranges); i++ {
		if ranges[i].lo <= ranges[i-1].hi {
			t.Fatalf("overlapping routes: %s and %s", routes[i-1], routes[i])
		}
	}
}

// routeCovers reports whether ip falls inside any of the given CIDR routes.
func routeCovers(routes []string, ip string) bool {
	target, err := ipToUint32(ip)
	if err != nil {
		panic(err)
	}
	for _, cidr := range routes {
		r, err := cidrToRange(cidr)
		if err != nil {
			panic(err)
		}
		if target >= r.lo && target <= r.hi {
			return true
		}
	}
	return false
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `docker run --rm -v "$PWD":/src -w /src golang:latest go test -buildvcs=false ./cmd/desktop/... -run TestPublicRoutes -v`
Expected: FAIL — `publicRoutes`, `ipv4Range`, `cidrToRange`, `ipToUint32` undefined.

- [ ] **Step 3: Write the implementation**

```go
// cmd/desktop/lanexclude.go
package main

import (
	"fmt"
	"math/bits"
	"sort"
	"strconv"
	"strings"
)

// privateIPv4CIDRs mirrors turn-proxy-android's PRIVATE_IPV4_CIDRS
// (WireGuardTunnelManager.kt) — kept identical rather than independently
// curated, so LAN devices (printer/NAS/router) stay reachable outside the
// tunnel the same way on every platform.
var privateIPv4CIDRs = []string{
	"10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16", "169.254.0.0/16",
	"127.0.0.0/8", "255.255.255.255/32", "224.0.0.0/4",
}

type ipv4Range struct{ lo, hi uint32 } // both inclusive

func ipToUint32(ip string) (uint32, error) {
	octets := strings.Split(ip, ".")
	if len(octets) != 4 {
		return 0, fmt.Errorf("lanexclude: malformed IPv4 %q", ip)
	}
	var v uint32
	for _, o := range octets {
		n, err := strconv.Atoi(o)
		if err != nil || n < 0 || n > 255 {
			return 0, fmt.Errorf("lanexclude: bad octet %q in %q", o, ip)
		}
		v = v<<8 | uint32(n)
	}
	return v, nil
}

func cidrToRange(cidr string) (ipv4Range, error) {
	parts := strings.SplitN(cidr, "/", 2)
	if len(parts) != 2 {
		return ipv4Range{}, fmt.Errorf("lanexclude: malformed CIDR %q", cidr)
	}
	base, err := ipToUint32(parts[0])
	if err != nil {
		return ipv4Range{}, err
	}
	prefix, err := strconv.Atoi(parts[1])
	if err != nil || prefix < 0 || prefix > 32 {
		return ipv4Range{}, fmt.Errorf("lanexclude: bad prefix in %q", cidr)
	}
	hostBits := 32 - prefix
	var size uint32 = 1
	if hostBits > 0 {
		size = uint32(1) << uint(hostBits)
	}
	return ipv4Range{lo: base, hi: base + size - 1}, nil
}

// mergeRanges sorts and coalesces overlapping/adjacent ranges.
func mergeRanges(ranges []ipv4Range) []ipv4Range {
	if len(ranges) == 0 {
		return nil
	}
	sorted := append([]ipv4Range(nil), ranges...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].lo < sorted[j].lo })
	merged := []ipv4Range{sorted[0]}
	for _, r := range sorted[1:] {
		last := &merged[len(merged)-1]
		if r.lo <= last.hi+1 {
			if r.hi > last.hi {
				last.hi = r.hi
			}
			continue
		}
		merged = append(merged, r)
	}
	return merged
}

// gaps returns the parts of the full IPv4 space [0, 2^32-1] not covered by
// excluded (must already be merged and sorted by mergeRanges).
func gaps(excluded []ipv4Range) []ipv4Range {
	var out []ipv4Range
	var cursor uint32
	for _, r := range excluded {
		if r.lo > cursor {
			out = append(out, ipv4Range{lo: cursor, hi: r.lo - 1})
		}
		if r.hi == 0xFFFFFFFF {
			return out
		}
		cursor = r.hi + 1
	}
	out = append(out, ipv4Range{lo: cursor, hi: 0xFFFFFFFF})
	return out
}

// rangeToCIDRs splits [lo,hi] into the minimal set of CIDR-aligned blocks.
func rangeToCIDRs(lo, hi uint32) []string {
	var out []string
	for {
		// alignBits: block size lo's own address alignment allows (32 = fully aligned, lo==0).
		alignBits := 32
		if lo != 0 {
			alignBits = bits.TrailingZeros32(lo)
		}
		// fitBits: largest power-of-two block size (as an exponent) that still fits in [lo,hi].
		fitBits := 32
		if !(lo == 0 && hi == 0xFFFFFFFF) {
			fitBits = bits.Len32(hi-lo+1) - 1
		}
		blockBits := alignBits
		if fitBits < blockBits {
			blockBits = fitBits
		}
		prefix := 32 - blockBits
		blockSize := uint32(1) << uint(blockBits)
		out = append(out, fmt.Sprintf("%d.%d.%d.%d/%d", byte(lo>>24), byte(lo>>16), byte(lo>>8), byte(lo), prefix))
		if hi-lo+1 == blockSize {
			break
		}
		lo += blockSize
	}
	return out
}

// publicRoutes returns the IPv4 public-internet address space as a minimal
// set of CIDR blocks: the full space minus privateIPv4CIDRs. Used as
// autoSystemRoutingTable for the tun xray inbound, so LAN devices stay
// reachable outside the tunnel (same policy as Android's
// RealityVpnService.excludeLanFromAllowedIps) instead of routing a blanket
// 0.0.0.0/0.
func publicRoutes() []string {
	var excluded []ipv4Range
	for _, c := range privateIPv4CIDRs {
		r, err := cidrToRange(c)
		if err != nil {
			// privateIPv4CIDRs is a fixed compile-time literal — a parse
			// failure here is a bug in this file, not a runtime condition.
			panic(fmt.Sprintf("lanexclude: invalid entry in privateIPv4CIDRs: %v", err))
		}
		excluded = append(excluded, r)
	}
	var out []string
	for _, g := range gaps(mergeRanges(excluded)) {
		out = append(out, rangeToCIDRs(g.lo, g.hi)...)
	}
	return out
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `docker run --rm -v "$PWD":/src -w /src golang:latest go test -buildvcs=false ./cmd/desktop/... -run TestPublicRoutes -v`
Expected: PASS (both `TestPublicRoutesExcludePrivateRanges` and `TestPublicRoutesDoNotOverlap`).

- [ ] **Step 5: gofmt and commit**

```bash
docker run --rm -v "$PWD":/src -w /src golang:latest gofmt -l cmd/desktop/lanexclude.go cmd/desktop/lanexclude_test.go
git add cmd/desktop/lanexclude.go cmd/desktop/lanexclude_test.go
git commit -m "feat(desktop): compute public-internet route list excluding LAN ranges"
```

---

### Task 2: Default-route physical interface detection

**Files:**
- Create: `cmd/desktop/netroute.go` (shared: injectable command runner)
- Create: `cmd/desktop/netroute_linux.go` (`//go:build linux`)
- Create: `cmd/desktop/netroute_windows.go` (`//go:build windows`)
- Test: `cmd/desktop/netroute_linux_test.go` (`//go:build linux`)

**Interfaces:**
- Consumes: nothing new.
- Produces: `defaultRouteInterface() (string, error)` — name of the OS's real physical uplink interface (e.g. `eth0`, `Ethernet`), used by Task 4/5 as `autoOutboundsInterface` so xray's own outbound sockets bypass the tun device it creates (avoids the routing loop documented in `proxy/tun`'s README).

- [ ] **Step 1: Write the failing test (Linux)**

```go
// cmd/desktop/netroute_linux_test.go
//go:build linux

package main

import (
	"errors"
	"testing"
)

func TestDefaultRouteInterfaceParsesIPRouteOutput(t *testing.T) {
	orig := runCommand
	defer func() { runCommand = orig }()
	runCommand = func(name string, args ...string) (string, error) {
		if name != "ip" {
			t.Fatalf("unexpected command %q", name)
		}
		return "default via 192.168.1.1 dev eth0 proto dhcp metric 100 \n", nil
	}

	iface, err := defaultRouteInterface()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if iface != "eth0" {
		t.Fatalf("got %q, want eth0", iface)
	}
}

func TestDefaultRouteInterfaceNoDefaultRoute(t *testing.T) {
	orig := runCommand
	defer func() { runCommand = orig }()
	runCommand = func(name string, args ...string) (string, error) {
		return "", nil // empty output: no default route configured
	}

	_, err := defaultRouteInterface()
	if !errors.Is(err, errNoDefaultRoute) {
		t.Fatalf("got err=%v, want errNoDefaultRoute", err)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `docker run --rm -v "$PWD":/src -w /src golang:latest go test -buildvcs=false ./cmd/desktop/... -run TestDefaultRouteInterface -v`
Expected: FAIL — `runCommand`, `defaultRouteInterface`, `errNoDefaultRoute` undefined.

- [ ] **Step 3: Write the shared command-runner file**

```go
// cmd/desktop/netroute.go
package main

import (
	"errors"
	"os/exec"
)

// errNoDefaultRoute is returned by defaultRouteInterface when the OS
// reports no default route at all (e.g. no network connectivity yet).
var errNoDefaultRoute = errors.New("netroute: no default route found")

// runCommand is a package-level var so tests can replace it with a canned
// fake instead of depending on a real network stack / real OS route table.
var runCommand = func(name string, args ...string) (string, error) {
	out, err := exec.Command(name, args...).CombinedOutput()
	return string(out), err
}
```

- [ ] **Step 4: Write the Linux implementation**

```go
// cmd/desktop/netroute_linux.go
//go:build linux

package main

import (
	"fmt"
	"regexp"
	"strings"
)

var defaultRouteDevRegexp = regexp.MustCompile(`\bdev\s+(\S+)`)

// defaultRouteInterface asks the kernel routing table for the interface the
// default (0.0.0.0/0) route goes out — the "real" physical/Wi-Fi uplink,
// as opposed to the tun device this process is about to create.
func defaultRouteInterface() (string, error) {
	out, err := runCommand("ip", "route", "show", "default")
	if err != nil {
		return "", fmt.Errorf("defaultRouteInterface: ip route show default: %w", err)
	}
	line := strings.TrimSpace(strings.SplitN(out, "\n", 2)[0])
	if line == "" {
		return "", errNoDefaultRoute
	}
	m := defaultRouteDevRegexp.FindStringSubmatch(line)
	if m == nil {
		return "", fmt.Errorf("defaultRouteInterface: could not parse %q", line)
	}
	return m[1], nil
}
```

- [ ] **Step 5: Write the Windows implementation** (not unit-testable in this Docker-only Linux environment — correct by inspection, verify manually per Task 5's live test)

```go
// cmd/desktop/netroute_windows.go
//go:build windows

package main

import (
	"fmt"
	"strings"
)

// defaultRouteInterface asks Windows for the interface alias the default
// (0.0.0.0/0) route goes out — the "real" physical/Wi-Fi uplink, as opposed
// to the wintun adapter this process is about to create.
func defaultRouteInterface() (string, error) {
	out, err := runCommand("powershell.exe", "-NoProfile", "-NonInteractive", "-Command",
		"(Get-NetRoute -DestinationPrefix '0.0.0.0/0' | Sort-Object -Property RouteMetric | "+
			"Select-Object -First 1 -ExpandProperty InterfaceAlias)")
	if err != nil {
		return "", fmt.Errorf("defaultRouteInterface: Get-NetRoute: %w", err)
	}
	iface := strings.TrimSpace(out)
	if iface == "" {
		return "", errNoDefaultRoute
	}
	return iface, nil
}
```

- [ ] **Step 6: Run test to verify it passes**

Run: `docker run --rm -v "$PWD":/src -w /src golang:latest go test -buildvcs=false ./cmd/desktop/... -run TestDefaultRouteInterface -v`
Expected: PASS (both cases).

- [ ] **Step 7: gofmt, vet, and commit**

```bash
docker run --rm -v "$PWD":/src -w /src golang:latest sh -c "gofmt -l cmd/desktop/netroute*.go && GOOS=linux go vet -buildvcs=false ./cmd/desktop/... && GOOS=windows go vet -buildvcs=false ./cmd/desktop/..."
git add cmd/desktop/netroute.go cmd/desktop/netroute_linux.go cmd/desktop/netroute_windows.go cmd/desktop/netroute_linux_test.go
git commit -m "feat(desktop): detect the physical default-route interface per OS"
```

---

### Task 3: Elevation check and self-relaunch

**Files:**
- Create: `cmd/desktop/elevate_linux.go` (`//go:build linux`)
- Create: `cmd/desktop/elevate_windows.go` (`//go:build windows`)
- Test: `cmd/desktop/elevate_linux_test.go` (`//go:build linux`)
- Modify: `go.mod` (add `golang.org/x/sys` as a direct dependency — currently indirect only)

**Interfaces:**
- Produces: `isElevated() bool`, `relaunchElevated(extraArgs []string) error` — same signatures on both platforms. Used by Task 5's `runVKTurnTunMode`.
  - **Linux semantics:** `relaunchElevated` blocks until the elevated child process exits (pkexec is synchronous); the caller should treat return-from-this-call as "the elevated run already happened, nothing more to do here."
  - **Windows semantics:** `relaunchElevated` returns as soon as the UAC-elevated process is *launched* (`ShellExecute`'s `runas` verb is fire-and-forget/detached); the caller should also just return right after — the new elevated process is now the one doing the real work, independently.

- [ ] **Step 1: Write the failing test (Linux — only the non-privileged detection path is testable without real root)**

```go
// cmd/desktop/elevate_linux_test.go
//go:build linux

package main

import "testing"

func TestIsElevatedFalseForNonRoot(t *testing.T) {
	// The CI/dev container and any normal dev machine run this test as a
	// non-root user — asserting isElevated() == true here would require the
	// test suite itself to run as root, which we don't want. This test only
	// pins the non-elevated path; the elevated path (euid==0) is exercised
	// manually per Task 5's live-test checklist, since faking os.Geteuid()
	// would just test a mock, not the real syscall.
	if isElevated() {
		t.Skip("test process is running as root — skipping, this path needs a non-root runner")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `docker run --rm -v "$PWD":/src -w /src golang:latest go test -buildvcs=false ./cmd/desktop/... -run TestIsElevated -v`
Expected: FAIL — `isElevated` undefined.

- [ ] **Step 3: Write the Linux implementation**

```go
// cmd/desktop/elevate_linux.go
//go:build linux

package main

import (
	"fmt"
	"os"
	"os/exec"
)

// isElevated reports whether this process already has root — Linux's only
// notion of "administrator" for the CAP_NET_ADMIN this mode needs to create
// a TUN device.
func isElevated() bool {
	return os.Geteuid() == 0
}

// relaunchElevated re-execs the current binary under pkexec (graphical
// polkit auth dialog) with extraArgs appended, and blocks until it exits.
// A declined/cancelled auth prompt returns a non-nil error.
func relaunchElevated(extraArgs []string) error {
	self, err := os.Executable()
	if err != nil {
		return fmt.Errorf("relaunchElevated: resolve self path: %w", err)
	}
	args := append([]string{self}, extraArgs...)
	cmd := exec.Command("pkexec", args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Stdin = os.Stdin
	return cmd.Run()
}
```

- [ ] **Step 4: Write the Windows implementation** (not unit-testable in this Docker-only Linux environment — correct by inspection, verify manually per Task 5's live test)

```go
// cmd/desktop/elevate_windows.go
//go:build windows

package main

import (
	"fmt"
	"os"
	"strings"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

// isElevated reports whether this process's token already carries
// administrator rights — Windows' precondition for creating a wintun
// adapter.
func isElevated() bool {
	return windows.GetCurrentProcessToken().IsElevated()
}

var (
	shell32           = windows.NewLazySystemDLL("shell32.dll")
	procShellExecuteW = shell32.NewProc("ShellExecuteW")
)

// relaunchElevated re-launches the current binary with the "runas" verb,
// which triggers the UAC consent prompt, passing extraArgs as a single
// space-joined command line. It returns as soon as the new process has been
// launched (or the UAC prompt was declined) — runas starts a detached
// process, so the caller should NOT wait on it; the elevated instance is now
// running independently and this (unprivileged) instance's job is done.
func relaunchElevated(extraArgs []string) error {
	self, err := os.Executable()
	if err != nil {
		return fmt.Errorf("relaunchElevated: resolve self path: %w", err)
	}
	verb, _ := syscall.UTF16PtrFromString("runas")
	file, _ := syscall.UTF16PtrFromString(self)
	params, _ := syscall.UTF16PtrFromString(strings.Join(extraArgs, " "))
	dir, _ := syscall.UTF16PtrFromString("")
	const swShowNormal = 1
	ret, _, _ := procShellExecuteW.Call(
		0,
		uintptr(unsafe.Pointer(verb)),
		uintptr(unsafe.Pointer(file)),
		uintptr(unsafe.Pointer(params)),
		uintptr(unsafe.Pointer(dir)),
		swShowNormal,
	)
	// ShellExecuteW returns a value > 32 on success; anything <= 32 is an
	// HINSTANCE-shaped error code (e.g. 5 = ERROR_ACCESS_DENIED when the
	// user declines the UAC prompt). See Win32 ShellExecute docs.
	if ret <= 32 {
		return fmt.Errorf("relaunchElevated: ShellExecuteW failed, code %d", ret)
	}
	return nil
}
```

- [ ] **Step 5: Add `golang.org/x/sys` as a direct dependency**

```bash
docker run --rm -v "$PWD":/src -w /src golang:latest sh -c "GOOS=windows go build -buildvcs=false ./cmd/desktop/... && go mod tidy"
```

- [ ] **Step 6: Run test to verify it passes**

Run: `docker run --rm -v "$PWD":/src -w /src golang:latest go test -buildvcs=false ./cmd/desktop/... -run TestIsElevated -v`
Expected: PASS (or SKIP if the container happens to run as root — both are acceptable outcomes per the test's own comment).

- [ ] **Step 7: Cross-compile check for Windows (this repo's desktop build is CGO_ENABLED=0 on the non-tray parts; a plain cross-compile is enough to catch syntax/API errors)**

```bash
docker run --rm -v "$PWD":/src -w /src golang:latest sh -c "GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go build -buildvcs=false -o /dev/null ./cmd/desktop/..."
```

Expected: builds cleanly (no systray CGO needed for this cross-compile check since `CGO_ENABLED=0` — this only proves the new Windows-tagged files compile, not that systray itself cross-compiles, which is a pre-existing, separate constraint of this codebase).

- [ ] **Step 8: gofmt and commit**

```bash
docker run --rm -v "$PWD":/src -w /src golang:latest gofmt -l cmd/desktop/elevate_linux.go cmd/desktop/elevate_windows.go cmd/desktop/elevate_linux_test.go
git add cmd/desktop/elevate_linux.go cmd/desktop/elevate_windows.go cmd/desktop/elevate_linux_test.go go.mod go.sum
git commit -m "feat(desktop): elevation check + self-relaunch for tun mode (UAC/pkexec)"
```

---

### Task 4: TUN xray config builder

**Files:**
- Modify: `cmd/desktop/launcher.go` (add alongside `buildVKTurnBridgeConfig`, which stays untouched)
- Test: `cmd/desktop/launcher_test.go` (add alongside existing tests)

**Interfaces:**
- Consumes: `debugMode` (existing package var), `vkTurnBridgeUUID` (existing const, same file).
- Produces: `buildVKTurnTunConfig(physicalInterface string, routes []string) (string, error)` and `const vkTurnTunInterfaceName = "vkturn0"`. Used by Task 5.

- [ ] **Step 1: Write the failing test**

```go
// append to cmd/desktop/launcher_test.go

func TestBuildVKTurnTunConfig(t *testing.T) {
	routes := []string{"8.0.0.0/7", "11.0.0.0/8"}
	raw, err := buildVKTurnTunConfig("eth0", routes)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var parsed struct {
		Inbounds []struct {
			Protocol string `json:"protocol"`
			Settings struct {
				Name                   string   `json:"name"`
				AutoOutboundsInterface string   `json:"autoOutboundsInterface"`
				AutoSystemRoutingTable []string `json:"autoSystemRoutingTable"`
			} `json:"settings"`
		} `json:"inbounds"`
		Outbounds []struct {
			Protocol string `json:"protocol"`
			Settings struct {
				Vnext []struct {
					Users []struct {
						ID string `json:"id"`
					} `json:"users"`
				} `json:"vnext"`
			} `json:"settings"`
		} `json:"outbounds"`
	}
	if err := json.Unmarshal([]byte(raw), &parsed); err != nil {
		t.Fatalf("buildVKTurnTunConfig produced invalid JSON: %v\n%s", err, raw)
	}

	if len(parsed.Inbounds) != 1 || parsed.Inbounds[0].Protocol != "tun" {
		t.Fatalf("expected exactly one tun inbound, got %+v", parsed.Inbounds)
	}
	in := parsed.Inbounds[0].Settings
	if in.Name != vkTurnTunInterfaceName {
		t.Errorf("interface name = %q, want %q", in.Name, vkTurnTunInterfaceName)
	}
	if in.AutoOutboundsInterface != "eth0" {
		t.Errorf("autoOutboundsInterface = %q, want eth0", in.AutoOutboundsInterface)
	}
	if !reflect.DeepEqual(in.AutoSystemRoutingTable, routes) {
		t.Errorf("autoSystemRoutingTable = %v, want %v", in.AutoSystemRoutingTable, routes)
	}

	if len(parsed.Outbounds) != 1 || parsed.Outbounds[0].Protocol != "vless" {
		t.Fatalf("expected exactly one vless outbound, got %+v", parsed.Outbounds)
	}
	gotUUID := parsed.Outbounds[0].Settings.Vnext[0].Users[0].ID
	if gotUUID != vkTurnBridgeUUID {
		t.Errorf("vless user id = %q, want vkTurnBridgeUUID (%q)", gotUUID, vkTurnBridgeUUID)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `docker run --rm -v "$PWD":/src -w /src golang:latest go test -buildvcs=false ./cmd/desktop/... -run TestBuildVKTurnTunConfig -v`
Expected: FAIL — `buildVKTurnTunConfig`, `vkTurnTunInterfaceName` undefined.

- [ ] **Step 3: Write the implementation**

Add to `cmd/desktop/launcher.go`, right after `buildVKTurnBridgeConfig` (import `"encoding/json"` at the top of the file if not already present — check first, `convertSubscription` in the same file already uses `json.Marshal`/`json.RawMessage`, so it's already imported):

```go
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
```

- [ ] **Step 4: Run test to verify it passes**

Run: `docker run --rm -v "$PWD":/src -w /src golang:latest go test -buildvcs=false ./cmd/desktop/... -run TestBuildVKTurnTunConfig -v`
Expected: PASS.

- [ ] **Step 5: gofmt and commit**

```bash
docker run --rm -v "$PWD":/src -w /src golang:latest gofmt -l cmd/desktop/launcher.go cmd/desktop/launcher_test.go
git add cmd/desktop/launcher.go cmd/desktop/launcher_test.go
git commit -m "feat(desktop): xray tun-inbound config builder for vk-turn (tun) mode"
```

---

### Task 5: `runVKTurnTunMode` orchestration

**Files:**
- Create: `cmd/desktop/tun.go`

**Interfaces:**
- Consumes: `isElevated`/`relaunchElevated` (Task 3), `defaultRouteInterface` (Task 2), `publicRoutes` (Task 1), `buildVKTurnTunConfig`/`vkTurnBridgeUUID`/`vkTurnIPCheckURL` (Task 4/existing), `resolveClientBin`/`resolveXrayBin`/`RunClient`/`RunXray`/`waitForListening`/`openOutputs`/`reportModeExit`/`startTray`/`restoreConsole` (existing, `launcher.go`/`main.go`/`tray.go`).
- Produces: `runVKTurnTunMode(ctx context.Context, cancel context.CancelFunc, dir string, cfg *DesktopConfig)`. Called by Task 6's `runMode`.

This task is integration glue over already-tested pieces (same as the existing `runVKTurnMode`, which also has no unit test — see its blast-radius note: real subprocesses and real elevation prompts aren't unit-testable). Verification is the manual golden-path checklist in Step 3.

- [ ] **Step 1: Write the implementation**

```go
// cmd/desktop/tun.go
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

// runVKTurnTunMode mirrors runVKTurnMode (main.go) but replaces the local
// SOCKS5 bridge with a system-wide TUN interface: once this reaches the
// "Подключено" state, the OS itself routes all traffic (minus the LAN
// exclusion list from lanexclude.go) through xray — no per-app proxy
// configuration needed. Requires admin/root, requested here (not earlier)
// so picking any other menu item never triggers a UAC/pkexec prompt.
func runVKTurnTunMode(ctx context.Context, cancel context.CancelFunc, dir string, cfg *DesktopConfig) {
	if !isElevated() {
		fmt.Println("Режиму 'vk-turn (tun)' нужны права администратора/root — запрашиваю...")
		if err := relaunchElevated([]string{"-tun-elevated"}); err != nil {
			fmt.Fprintln(os.Stderr, "Не удалось получить права:", err)
			return
		}
		// Linux: relaunchElevated already blocked until the elevated child
		// finished — nothing left to do. Windows: the elevated process is
		// now running independently — this (unprivileged) instance is done.
		return
	}

	iface, err := defaultRouteInterface()
	if err != nil {
		fmt.Fprintln(os.Stderr, "Не удалось определить сетевой интерфейс:", err)
		return
	}
	routes := publicRoutes()

	clientBin, err := resolveClientBin(dir)
	if err != nil {
		fmt.Fprintln(os.Stderr, "Не найден client:", err)
		return
	}
	xrayBin, err := resolveXrayBin(dir)
	if err != nil {
		fmt.Fprintln(os.Stderr, "Не найден xray:", err)
		return
	}

	stdout, stderr, closeOutputs, err := openOutputs()
	if err != nil {
		fmt.Fprintln(os.Stderr, "Не удалось открыть debug-лог:", err)
		return
	}
	defer closeOutputs()

	clientDone := make(chan error, 1)
	go func() { clientDone <- RunClient(ctx, clientBin, cfg, stdout, stderr) }()

	fmt.Println("Поднимаю туннель VK-TURN...")
	const clientListenTimeout = 60 * time.Second
	if err := waitForListening(ctx, "127.0.0.1:9000", clientListenTimeout); err != nil {
		fmt.Fprintln(os.Stderr, "Туннель не поднялся:", err)
		cancel()
		<-clientDone
		return
	}

	tunConfig, err := buildVKTurnTunConfig(iface, routes)
	if err != nil {
		fmt.Fprintln(os.Stderr, "Не удалось собрать конфиг tun:", err)
		cancel()
		<-clientDone
		return
	}

	xrayDone := make(chan error, 1)
	go func() { xrayDone <- RunXray(ctx, xrayBin, tunConfig, stdout, stderr) }()

	// tun mode opens no local port to poll (unlike the SOCKS bridge's
	// waitForListening) — give xray a fixed grace period to create the
	// interface and apply routes before running the connectivity check.
	select {
	case <-time.After(3 * time.Second):
	case err := <-xrayDone:
		fmt.Fprintln(os.Stderr, "xray завершился при поднятии tun:", err)
		cancel()
		<-clientDone
		return
	case <-ctx.Done():
		<-clientDone
		return
	}

	checkCtx, checkCancel := context.WithTimeout(ctx, 10*time.Second)
	ip, err := checkDirectConnectivity(checkCtx)
	checkCancel()
	if err != nil {
		fmt.Fprintln(os.Stderr, "Не удалось проверить соединение (туннель может не работать):", err)
	} else {
		fmt.Println("Подключено, выходной IP:", ip)
	}
	fmt.Println("Весь трафик машины теперь идёт через тоннель. Ctrl+C — остановить.")

	startTray(ctx, cancel, "Подключено (tun)")
	defer restoreConsole()

	select {
	case err := <-clientDone:
		reportModeExit(ctx, "client", err)
		cancel()
		<-xrayDone
	case err := <-xrayDone:
		reportModeExit(ctx, "xray", err)
		cancel()
		<-clientDone
	}
}

// checkDirectConnectivity is checkVKTurnConnectivity's tun-mode counterpart:
// tun mode has no local SOCKS port to dial through deliberately — the OS
// itself now routes a plain HTTP client's connection through the tunnel, so
// this just uses http.DefaultClient's normal dial path instead of a SOCKS5
// dialer.
func checkDirectConnectivity(ctx context.Context) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, vkTurnIPCheckURL, nil)
	if err != nil {
		return "", err
	}
	client := &http.Client{Timeout: 10 * time.Second}
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
```

- [ ] **Step 2: Build check**

```bash
docker run --rm -v "$PWD":/src -w /src golang:latest sh -c "go build -buildvcs=false ./cmd/desktop/... && GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go build -buildvcs=false -o /dev/null ./cmd/desktop/..."
```

Expected: both builds succeed.

- [ ] **Step 3: Manual golden-path test (Linux — run on a real machine, not this container; needs a real network + real desktop kit)**

1. Build a debug kit exactly like the one already used earlier this session (client + vkturn-desktop + xray in one directory, `VKTURN_DEBUG=1`).
2. Run `./vkturn-desktop`, select `vk-turn (tun)` as a normal (non-root) user.
3. Confirm the pkexec graphical auth dialog appears; enter the password.
4. Confirm the elevated process prints "Поднимаю туннель VK-TURN...", then "Подключено, выходной IP: ...".
5. From a *second* terminal (unelevated, doesn't matter), run `curl https://api.ipify.org` and confirm it matches the IP printed in step 4 (not the machine's real ISP-assigned IP) — this is the project's required positive control (CLAUDE.md "Метод").
6. Confirm LAN devices are still reachable: `ping <router LAN IP>` succeeds.
7. Ctrl+C the desktop app; confirm the `vkturn0` interface disappears (`ip addr show vkturn0` → "does not exist") and `curl https://api.ipify.org` reverts to the real ISP IP.

- [ ] **Step 4: Commit**

```bash
git add cmd/desktop/tun.go
git commit -m "feat(desktop): runVKTurnTunMode — orchestrate client+xray-tun with elevation"
```

---

### Task 6: Menu wiring

**Files:**
- Modify: `cmd/desktop/main.go`

**Interfaces:**
- Consumes: `runVKTurnTunMode` (Task 5).
- Produces: `-tun-elevated` CLI flag, new menu entries `"vk-turn (socks)"` and `"vk-turn (tun)"`, new `runMode` case `"vk-turn-tun"`.

- [ ] **Step 1: Add the flag and menu wiring**

In `cmd/desktop/main.go`, add `"flag"` to the import block if not already present, then:

```go
// tunElevated is set by relaunchElevated (elevate_linux.go/elevate_windows.go)
// when re-execing this binary with a UAC/pkexec prompt already granted —
// skips straight to vk-turn (tun) instead of showing the interactive menu
// again in the elevated process.
var tunElevated = flag.Bool("tun-elevated", false,
	"внутренний флаг: пропустить меню, сразу поднять vk-turn (tun) (используется relaunchElevated)")
```

Then in `main()`, add `flag.Parse()` as the very first line, and branch before the menu loop:

```go
func main() {
	flag.Parse()

	cfg, err := LoadCache()
	if err != nil {
		cfg, err = loginWithRetries()
		if err != nil {
			fmt.Fprintln(os.Stderr, "Не удалось войти:", err)
			os.Exit(1)
		}
	}

	if *tunElevated {
		runMode(cfg, "vk-turn-tun")
		return
	}

	for {
		choice, err := RunMenu([]string{"vk-turn (socks)", "vk-turn (tun)", "xray-подписка", "обновить конфиг", "выход"})
		if err != nil {
			fmt.Fprintln(os.Stderr, "\nМеню прервано:", err)
			return
		}
		switch choice {
		case "vk-turn (socks)":
			runMode(cfg, "vk-turn")
		case "vk-turn (tun)":
			runMode(cfg, "vk-turn-tun")
		case "xray-подписка":
			runMode(cfg, "xray")
		case "обновить конфиг":
			refreshed, err := loginFlow()
			if err != nil {
				fmt.Fprintln(os.Stderr, "Не удалось обновить конфиг:", err)
				continue
			}
			cfg = refreshed
		case "выход":
			return
		}
	}
}
```

And in `runMode`'s switch:

```go
	switch mode {
	case "vk-turn":
		runVKTurnMode(ctx, cancel, dir, cfg)
	case "vk-turn-tun":
		runVKTurnTunMode(ctx, cancel, dir, cfg)
	case "xray":
		runXraySubscriptionMode(ctx, cancel, dir, cfg)
	}
```

- [ ] **Step 2: Build check**

```bash
docker run --rm -v "$PWD":/src -w /src golang:latest sh -c "go build -buildvcs=false ./cmd/desktop/... && go vet -buildvcs=false ./cmd/desktop/..."
```

Expected: builds and vets cleanly.

- [ ] **Step 3: Run the full existing desktop test suite to confirm nothing else broke**

```bash
docker run --rm -v "$PWD":/src -w /src golang:latest go test -buildvcs=false ./cmd/desktop/... -v
```

Expected: all tests pass, including the new ones from Tasks 1, 2, 3, 4.

- [ ] **Step 4: gofmt and commit**

```bash
docker run --rm -v "$PWD":/src -w /src golang:latest gofmt -l cmd/desktop/main.go
git add cmd/desktop/main.go
git commit -m "feat(desktop): wire vk-turn (tun) into the menu and -tun-elevated relaunch flag"
```

---

### Task 7: Windows `wintun.dll` release packaging

**Files:**
- Modify: `.github/workflows/release.yml`
- Modify: `.goreleaser.yaml`
- Modify: `docs/flags.md` (document the new menu option and its admin/root requirement)

This automates what Task 5's manual test already does by hand (dropping `wintun.dll` next to the binaries) so the family-facing kit-assembly step (already manual today per `docs/superpowers/specs/2026-08-14-desktop-client-design.md`) has the file available on the release page, the same way `xray-dist/xray-windows-amd64.exe` already is. This task doesn't block using/testing the feature — Task 5's manual test already covers a working build without it (Linux doesn't need `wintun.dll` at all; a Windows test build just needs the DLL copied by hand once).

- [ ] **Step 1: Determine and pin the wintun.dll version**

This can't be done from a sandboxed environment without live internet access. On a machine with network access, run:

```bash
curl -fsSL -o /tmp/wintun.zip https://www.wintun.net/builds/wintun-0.14.1.zip
sha256sum /tmp/wintun.zip
```

Cross-check the printed hash against the one published on `https://www.wintun.net` for that build (the site publishes SHA256 sums per release). If `0.14.1` is no longer the latest stable build by the time this task is executed, use whatever the site currently lists as latest instead, and use *that* build's published hash — do not skip the cross-check.

- [ ] **Step 2: Add the fetch step to `release.yml`**

Add a step near the existing "Build xray CLI" step (the one staging `xray-dist/xray-windows-amd64.exe`, `.github/workflows/release.yml:178-182`), in the same job:

```yaml
      - name: Fetch wintun.dll
        run: |
          mkdir -p wintun-dist
          curl -fsSL -o /tmp/wintun.zip https://www.wintun.net/builds/wintun-0.14.1.zip
          echo "<hash-from-step-1>  /tmp/wintun.zip" | sha256sum -c -
          unzip -p /tmp/wintun.zip wintun/bin/amd64/wintun.dll > wintun-dist/wintun.dll
```

Replace `<hash-from-step-1>` with the real hash obtained and cross-checked in Step 1 — this is a hard requirement (`sha256sum -c` fails the build otherwise), not a placeholder to leave in.

- [ ] **Step 3: Add it to `.goreleaser.yaml`'s `release.extra_files`**

In `.goreleaser.yaml`, next to the existing `xray-dist/xray-windows-amd64.exe` entry (around line 196):

```yaml
    - glob: xray-dist/xray-windows-amd64.exe
    - glob: wintun-dist/wintun.dll
```

- [ ] **Step 4: Document the new mode in `docs/flags.md`**

Add a short section (match the file's existing style/heading level for desktop-mode documentation) noting: `vk-turn (tun)` requires administrator/root (prompts automatically via UAC/pkexec when selected), captures all machine traffic except the LAN/private ranges, Windows builds need `wintun.dll` next to `vkturn-desktop.exe`/`xray.exe` (now shipped as a release asset, see the kit-assembly process), macOS is not supported yet.

- [ ] **Step 5: Commit**

```bash
git add .github/workflows/release.yml .goreleaser.yaml docs/flags.md
git commit -m "ci(desktop): ship wintun.dll as a release asset for vk-turn (tun)"
```

---

## Self-Review Notes

- **Spec coverage:** menu split (Task 6) ✓, xray `proxy/tun` reuse with `autoOutboundsInterface`/`autoSystemRoutingTable` (Task 4) ✓, elevation only on tun selection (Task 3, Task 5 step 1) ✓, LAN exclusion mirroring Android (Task 1) ✓, error handling for elevation refusal / xray failure / interface-detection failure (Task 5) ✓, testing plan incl. positive control (Task 5 Step 3) ✓, wintun.dll distribution risk (Task 7) ✓. IPv6/macOS explicitly out of scope per spec, not implemented — correct, matches "Не-цели".
- **Type consistency checked:** `defaultRouteInterface() (string, error)` (Task 2) matches its use in Task 5. `publicRoutes() []string` (Task 1) matches Task 4/5. `buildVKTurnTunConfig(string, []string) (string, error)` consistent across Task 4 definition and Task 5 call site. `isElevated() bool` / `relaunchElevated([]string) error` identical signatures in both `elevate_linux.go` and `elevate_windows.go` (Task 3), matching Task 5's call sites.
- **The `rangeToCIDRs`/`mergeRanges`/`gaps` algorithm in Task 1 was written and run standalone (not just hand-derived) against 10 known IPs (7 private/reserved, 3 public including a real TURN relay IP from this session's logs) plus an overlap check across all 72 resulting routes — all passed before being placed in this plan.
