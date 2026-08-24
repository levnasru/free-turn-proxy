# UDP-relay session affinity — implementation plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Stop `-transport udp` from flapping WireGuard's endpoint by routing all inbound packets through a single dispatcher-owned active slot instead of N independent streams racing to read a shared channel, while keeping only a small hot-set of live TURN allocations instead of holding all N open at once.

**Architecture:** A new `dispatcher` becomes the sole reader of `udprelay`'s existing `inboundChan` and owns a pointer to the "active" member of a K-sized hot-set (K << today's N). It routes every packet to that one member, rotates the pointer by accumulated volume (not per-packet, not by timer), and fails over immediately if the active member stops accepting packets. A new `sessionManager` replaces `Run()`'s `for i := range numStreams { go maintainSession... }` loop: it launches and retires hot-set members over time, reusing `DTLSLoop`/`TURNLoop`/`oneDTLS`/`oneTURN`/`common.DialTURN` completely unchanged — each member just gets its own small inbound channel instead of the shared one, and the dispatcher is the only thing new sitting between the listener and the existing per-stream loop.

**Tech Stack:** Go 1.26 (this repo's `go.mod`), stdlib only (`context`, `sync`, `time`, `net`, `bufio`), no new dependencies.

**Spec:** `docs/superpowers/specs/2026-08-23-udp-relay-session-affinity-design.md` (approved, committed as `e3488b5`) — this plan implements it; read both together.

## Deviation from the spec, flagged explicitly

The spec's Component 1 describes pre-fetching a tagged candidate pool (call
`GetCredentials` once per provider, tag each returned address with
`(providerIdx, addrIdx)`, group it, then launch each hot-set slot pinned to
a specifically chosen candidate). Implementing "pinned to a specifically
chosen candidate" without touching `DialTURN`/`common.candidateIdx`
requires picking a `streamID` for which their existing formula
(`streamID % addrCount`, composed with `multi.Provider`'s
`(streamID-1) % providerCount`) resolves to that exact candidate as its
first try. That mapping is **not always reachable**: when
`gcd(providerCount, addrCount) > 1` (e.g. 2 accounts × 2 addresses each),
only half of the `(provider, address)` pairs have any `streamID` that hits
them first — a real correctness gap, not a rare edge case, given the
project's actual 1-2-account / 1-2-address-per-account profile.

This plan replaces that with an **observe-then-adapt** design instead:
hot-set slots launch with plain sequential `streamID`s (1, 2, 3, ...) —
exactly what `Run()` already did for all N streams before this change,
which is proven to spread across providers/addresses via the existing,
unmodified `multi.Provider` + `candidateIdx` machinery. Each slot reports
which relay IP it actually landed on via the new `Params.OnAllocated` hook
(Task 4); `sessionManager.refreshOne` (Task 8) uses that observed data to
retire over-represented members over time (`pickReplacementCandidate`,
Task 2). This drops `taggedAddr`, `groupByPrefix24` (the pool-grouping
function - `groupPrefix24`, singular-address, is kept), `selectHotSet`, and
any pre-connection candidate targeting from the spec's literal component
list. It keeps the spec's actual goals (bounded live allocations, diversity
pressure over time, no `DialTURN`/`candidateIdx`/`multi.Provider` changes)
without the reachability gap.

**Real trade-off this introduces:** the *initial* hot-set (launched at
startup, Task 7) has no diversity guarantee — it's whatever the first K
sequential `streamID`s happen to land on. Diversity only improves reactively,
one `refreshOne` replacement at a time, starting after the first
`hotSetRefreshInterval` tick (5 minutes by the starting constant). If this
turns out too slow in the live before/after measurement, the fix is
narrowing `hotSetRefreshInterval` for the first few cycles after startup,
not resurrecting the streamID-pinning approach.

## Global Constraints

- No server-side changes. `internal/proxy/udpserver` stays exactly as-is (per-stream, unaware of "active").
- `internal/proxy/tcpfwd`/`bondclient`/`bondserver` (TCP+bond mode) are untouched — this is a `udprelay`-only change.
- `DTLSLoop`, `TURNLoop`, `oneDTLS`, `oneTURN`, `common.DialTURN`, `common.candidateIdx`, `internal/provider/multi.Provider` keep their exact current logic — every new capability is wired from the *outside* (new callers, new optional `Params` fields checked for `nil` exactly like the existing `TrafficStats` field), never by editing their control flow.
- `-n` is reused for the new hot-set size K (not a new flag, not deprecated) — this was already decided in the spec body, not left open. It's a semantic change for `-transport udp` only; `docs/flags.md` must say so, and it ships in the project's next *batch* client release, not as a standalone push (see `CLAUDE.md`, "Релиз клиентов пачкой").
- The `/24` grouping is a heuristic, not verified VK topology: every place that uses it must degrade to "no worse than random" if the grouping turns out meaningless — never treat it as ground truth.
- Two constants are explicitly **not calibrated** by this plan (the spec leaves them for empirical tuning): `rotateThresholdBytes` and `hotSetRefreshInterval`. Both get a documented starting value and a comment saying so — no task in this plan is "measure and set the real value."

## File Structure

- `internal/proxy/udprelay/candidate.go` (new) — three pure functions: `groupPrefix24` (/24 key for one address), `pickReplacementCandidate` (which hot-set member to retire at a refresh tick), `shouldRefresh` (timer gate). No state, no I/O.
- `internal/proxy/udprelay/dispatcher.go` (new) — `slotHandle` (channels a hot-set member exposes) and `dispatcher` (routes packets to the active slot, rotates by volume/manual trigger/liveness failure). Pure in-memory logic, testable with fake `slotHandle`s and no real network.
- `internal/proxy/udprelay/sessionmgr.go` (new) — `sessionManager`: owns the hot-set's lifecycle (launch, barrier, periodic refresh), wires real `DTLSLoop`/`TURNLoop` goroutines into `slotHandle`s and into the `dispatcher`. This is where "who launches oneDTLS and how many" lives now, replacing that logic in `Run()`.
- `internal/proxy/udprelay/loop.go` (modify) — two-line additive hook in `oneTURN` calling `params.OnAllocated`, same pattern as the existing `params.TrafficStats` calls right next to it.
- `internal/proxy/udprelay/run.go` (modify) — `Params` gains `OnAllocated` and `RotateCh`; `Run()`'s per-stream loop is replaced by constructing and running a `sessionManager`; the `numStreams` parameter is renamed `hotSetK` (same position, same type, new meaning).
- `cmd/client/main.go` (modify) — reads `rotate\n` lines from stdin (UDP mode only) and forwards them to the new `RotateCh`.
- `mobile/mobile.go` (modify) — exports `TriggerRotate()` for iOS/gomobile in-process use; mirrors the `cmd/client/main.go` call-site change (this file keeps its own `buildProvider`/`Run()`-call copy per this project's existing gomobile-bind constraint, see repo `CLAUDE.md`).
- `docs/flags.md` (modify) — documents `-n`'s new UDP-mode meaning.

## Global note on test coverage

`internal/proxy/udprelay` currently has **zero test files** — `DTLSLoop`, `TURNLoop`, `Run` are exercised only by live runs, never unit tests (confirmed via `go test ./...`: `? .../udprelay [no test files]`). This plan follows that existing precedent: the three new pure functions (candidate.go) and the dispatcher's in-memory routing logic (dispatcher.go) get real unit tests, because they don't need a network to exercise. `sessionmgr.go`'s orchestration (which launches real goroutines against real TURN servers) gets none, matching how `Run()` itself has never had one — it's validated by the project's existing "measure, don't guess" live-testing norm, not by this plan.

---

### Task 1: `groupPrefix24` — /24 grouping key for one relay address

**Files:**
- Create: `internal/proxy/udprelay/candidate.go`
- Test: `internal/proxy/udprelay/candidate_test.go`

**Interfaces:**
- Produces: `func groupPrefix24(hostport string) string` — used by Task 2's `pickReplacementCandidate` caller (`sessionManager.refreshOne`, Task 8) to turn a resolved `*net.UDPAddr` into a group key.

- [ ] **Step 1: Write the failing test**

```go
package udprelay

import "testing"

func TestGroupPrefix24(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		hostport string
		want    string
	}{
		{"ipv4 with port", "203.0.113.42:3478", "203.0.113"},
		{"ipv4 different last octet still same group", "203.0.113.7:443", "203.0.113"},
		{"ipv4 different /24", "203.0.114.7:3478", "203.0.114"},
		{"no port falls back to raw host", "203.0.113.42", "203.0.113.42"},
		{"unresolved hostname falls back to itself", "relay.example.internal:3478", "relay.example.internal:3478"},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			if got := groupPrefix24(c.hostport); got != c.want {
				t.Errorf("groupPrefix24(%q) = %q, want %q", c.hostport, got, c.want)
			}
		})
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `docker run --rm -v "$(pwd)":/src -w /src golang:1.26.5 go test ./internal/proxy/udprelay/... -run TestGroupPrefix24 -v`
Expected: FAIL — `undefined: groupPrefix24` (build failure, since `candidate.go` doesn't exist yet)

- [ ] **Step 3: Write the implementation**

```go
package udprelay

import (
	"fmt"
	"net"
)

// groupPrefix24 returns the /24 group key for a "host:port" relay address:
// the first three octets of an IPv4 host. Non-IPv4 hosts (unresolved
// hostnames, IPv6, or anything net.SplitHostPort can't parse) fall back to
// the raw input string as their own singleton group - grouping degrades to
// a no-op instead of erroring, matching the spec's "not worse than random"
// ceiling (docs/superpowers/specs/2026-08-23-udp-relay-session-affinity-design.md,
// "Не цели").
func groupPrefix24(hostport string) string {
	host, _, err := net.SplitHostPort(hostport)
	if err != nil {
		host = hostport
	}
	ip := net.ParseIP(host).To4()
	if ip == nil {
		return hostport
	}
	return fmt.Sprintf("%d.%d.%d", ip[0], ip[1], ip[2])
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `docker run --rm -v "$(pwd)":/src -w /src golang:1.26.5 go test ./internal/proxy/udprelay/... -run TestGroupPrefix24 -v`
Expected: PASS (all 5 subtests)

- [ ] **Step 5: Commit**

```bash
git add internal/proxy/udprelay/candidate.go internal/proxy/udprelay/candidate_test.go
git commit -m "feat(udprelay): groupPrefix24 - /24 grouping key for relay addresses"
```

---

### Task 2: `pickReplacementCandidate` — which hot-set member to retire

**Files:**
- Modify: `internal/proxy/udprelay/candidate.go`
- Test: `internal/proxy/udprelay/candidate_test.go`

**Interfaces:**
- Consumes: nothing outside stdlib.
- Produces: `func pickReplacementCandidate(streamIDs []int, activeStreamID int, groups map[int]string) int` — returns the streamID to retire, or `-1` if nothing is clearly over-represented. Used by `sessionManager.refreshOne` (Task 8).

- [ ] **Step 1: Write the failing test**

```go
func TestPickReplacementCandidate(t *testing.T) {
	t.Parallel()

	t.Run("no groups known yet - nothing to retire", func(t *testing.T) {
		t.Parallel()
		got := pickReplacementCandidate([]int{1, 2, 3}, 1, map[int]string{})
		if got != -1 {
			t.Errorf("got %d, want -1", got)
		}
	})

	t.Run("all groups distinct - nothing over-represented", func(t *testing.T) {
		t.Parallel()
		groups := map[int]string{1: "a", 2: "b", 3: "c"}
		got := pickReplacementCandidate([]int{1, 2, 3}, 1, groups)
		if got != -1 {
			t.Errorf("got %d, want -1", got)
		}
	})

	t.Run("one group has two members - retires the non-active one", func(t *testing.T) {
		t.Parallel()
		groups := map[int]string{1: "a", 2: "a", 3: "b"}
		got := pickReplacementCandidate([]int{1, 2, 3}, 1, groups)
		if got != 2 {
			t.Errorf("got %d, want 2 (streamID 1 is active, must not be retired)", got)
		}
	})

	t.Run("active slot's own group never retires the active slot", func(t *testing.T) {
		t.Parallel()
		groups := map[int]string{1: "a", 2: "a", 3: "b"}
		got := pickReplacementCandidate([]int{1, 2, 3}, 2, groups)
		if got != 1 {
			t.Errorf("got %d, want 1 (streamID 2 is active, must not be retired)", got)
		}
	})

	t.Run("picks the most over-represented group", func(t *testing.T) {
		t.Parallel()
		groups := map[int]string{1: "a", 2: "a", 3: "a", 4: "b", 5: "b"}
		got := pickReplacementCandidate([]int{1, 2, 3, 4, 5}, 1, groups)
		if got != 2 && got != 3 {
			t.Errorf("got %d, want one of the non-active members of the 3-strong group a (2 or 3)", got)
		}
	})
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `docker run --rm -v "$(pwd)":/src -w /src golang:1.26.5 go test ./internal/proxy/udprelay/... -run TestPickReplacementCandidate -v`
Expected: FAIL — `undefined: pickReplacementCandidate`

- [ ] **Step 3: Write the implementation**

Append to `internal/proxy/udprelay/candidate.go`:

```go
// pickReplacementCandidate chooses which non-active hot-set member to
// retire at the next refresh tick: the member whose relay's /24 group has
// the most OTHER representatives currently in the hot-set (over-represented
// groups first), so refresh churn actively pushes toward group diversity
// instead of picking at random. groups maps streamID -> its relay's /24 key;
// a streamID missing from groups (still connecting, or connection failed
// before Params.OnAllocated fired) is never picked - retiring a slot we
// know nothing about yet would be guessing, not measuring. Returns -1 if no
// eligible member exists (empty hot-set, every known group has exactly one
// representative, or no group memberships are known yet).
func pickReplacementCandidate(streamIDs []int, activeStreamID int, groups map[int]string) int {
	counts := make(map[string]int, len(streamIDs))
	for _, id := range streamIDs {
		if g, ok := groups[id]; ok {
			counts[g]++
		}
	}

	best := -1
	bestCount := 1 // только группы с >=2 представителями считаются избыточными
	for _, id := range streamIDs {
		if id == activeStreamID {
			continue
		}
		g, ok := groups[id]
		if !ok {
			continue
		}
		if counts[g] > bestCount {
			bestCount = counts[g]
			best = id
		}
	}
	return best
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `docker run --rm -v "$(pwd)":/src -w /src golang:1.26.5 go test ./internal/proxy/udprelay/... -run TestPickReplacementCandidate -v`
Expected: PASS (all 5 subtests)

- [ ] **Step 5: Commit**

```bash
git add internal/proxy/udprelay/candidate.go internal/proxy/udprelay/candidate_test.go
git commit -m "feat(udprelay): pickReplacementCandidate - retire over-represented hot-set members"
```

---

### Task 3: `shouldRefresh` — refresh timer gate

**Files:**
- Modify: `internal/proxy/udprelay/candidate.go`
- Test: `internal/proxy/udprelay/candidate_test.go`

**Interfaces:**
- Produces: `func shouldRefresh(lastRefresh, now time.Time, interval time.Duration) bool` — used by `sessionManager.run`'s refresh loop (Task 8). A plain function taking `now` as a parameter (not a real sleep, not a clock interface) is this repo's existing pattern for testing time-driven logic without waiting — see `internal/clientsdb/db_test.go`, which tests hot-reload by directly rewinding `db2.lastModified` rather than sleeping or injecting a clock type.

- [ ] **Step 1: Write the failing test**

```go
func TestShouldRefresh(t *testing.T) {
	t.Parallel()
	base := time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC)
	interval := 5 * time.Minute

	cases := []struct {
		name string
		now  time.Time
		want bool
	}{
		{"before interval elapsed", base.Add(4 * time.Minute), false},
		{"exactly at interval", base.Add(5 * time.Minute), true},
		{"past interval", base.Add(10 * time.Minute), true},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			if got := shouldRefresh(base, c.now, interval); got != c.want {
				t.Errorf("shouldRefresh(%v, %v, %v) = %v, want %v", base, c.now, interval, got, c.want)
			}
		})
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `docker run --rm -v "$(pwd)":/src -w /src golang:1.26.5 go test ./internal/proxy/udprelay/... -run TestShouldRefresh -v`
Expected: FAIL — `undefined: shouldRefresh`

- [ ] **Step 3: Write the implementation**

Append to `internal/proxy/udprelay/candidate.go` (add `"time"` to the import block):

```go
// shouldRefresh reports whether interval has elapsed since lastRefresh, as
// of now. A plain function of three values, not a stateful ticker wrapper -
// keeps the refresh decision itself testable without a real clock or sleep.
func shouldRefresh(lastRefresh, now time.Time, interval time.Duration) bool {
	return !now.Before(lastRefresh.Add(interval))
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `docker run --rm -v "$(pwd)":/src -w /src golang:1.26.5 go test ./internal/proxy/udprelay/... -run TestShouldRefresh -v`
Expected: PASS (all 3 subtests)

- [ ] **Step 5: Commit**

```bash
git add internal/proxy/udprelay/candidate.go internal/proxy/udprelay/candidate_test.go
git commit -m "feat(udprelay): shouldRefresh - hot-set refresh timer gate"
```

---

### Task 4: `Params.OnAllocated` / `Params.RotateCh` — additive hooks

**Files:**
- Modify: `internal/proxy/udprelay/run.go` (`Params` struct)
- Modify: `internal/proxy/udprelay/loop.go` (`oneTURN`)

**Interfaces:**
- Produces: `Params.OnAllocated func(streamID int, relayAddr *net.UDPAddr)` and `Params.RotateCh <-chan struct{}`, both nil-safe. Consumed by `sessionManager` (Task 7/8, sets `OnAllocated`) and by `dispatcher.run` (Task 5, reads `RotateCh`) and by `cmd/client/main.go`/`mobile/mobile.go` (Tasks 10/11, which construct and populate `RotateCh`).

No test for this task — see "Global note on test coverage" above: `oneTURN` requires a real TURN server to exercise, exactly like the existing `TrafficStats.AddTx/AddRx` calls two lines below the new one, which also have no dedicated test.

- [ ] **Step 1: Add the two fields to `Params`**

In `internal/proxy/udprelay/run.go`, extend the `Params` struct:

```go
// Params - per-stream конфигурация TURN/wrap, общая для DTLS и TURN циклов.
type Params struct {
	Host         string
	Port         string
	TransportUDP bool
	Profile      string
	ObfKey       []byte
	ObfTiming    time.Duration
	GetCreds     GetCredsFunc
	ClientID     string
	TrafficStats *stats.Stats

	// OnAllocated, если задан, вызывается один раз сразу после успешного
	// TURN-allocate для потока streamID с адресом реального relay-сервера,
	// на который он сел. sessionManager использует это для /24-группировки
	// при обновлении состава hot-set'а (см. pickReplacementCandidate и
	// docs/superpowers/specs/2026-08-23-udp-relay-session-affinity-design.md).
	// nil - no-op, как и TrafficStats.
	OnAllocated func(streamID int, relayAddr *net.UDPAddr)

	// RotateCh, если задан, немедленно переключает активный слот hot-set'а
	// на следующего кандидата при получении сигнала - ручной триггер для
	// пользователя, когда деградация видна, но не ловится liveness-проверкой
	// (см. ту же спеку, "Failover"). nil - ручного переключения нет
	// (TCP+bond режим его не использует).
	RotateCh <-chan struct{}
}
```

- [ ] **Step 2: Call `OnAllocated` from `oneTURN`**

In `internal/proxy/udprelay/loop.go`, inside `oneTURN`, right after the existing log line that already has the address in scope:

```go
	relayConn := stream.Relay
	deps.log().Debugf("[STREAM %d] TURN server IP: %s", streamID, stream.ServerUDPAddr.IP)
	if params.OnAllocated != nil {
		params.OnAllocated(streamID, stream.ServerUDPAddr)
	}
```

- [ ] **Step 3: Build to confirm no regressions**

Run: `docker run --rm -v "$(pwd)":/src -w /src golang-ayatana:latest sh -c "git config --global --add safe.directory /src; go build -buildvcs=false ./... && go test -buildvcs=false ./..."`
Expected: `BUILD_OK`, all packages `ok` or `[no test files]`, none `FAIL`

- [ ] **Step 4: Commit**

```bash
git add internal/proxy/udprelay/run.go internal/proxy/udprelay/loop.go
git commit -m "feat(udprelay): additive OnAllocated/RotateCh hooks in Params"
```

---

### Task 5: `slotHandle` + dispatcher core routing (manual + volume rotation)

**Files:**
- Create: `internal/proxy/udprelay/dispatcher.go`
- Test: `internal/proxy/udprelay/dispatcher_test.go`

**Interfaces:**
- Consumes: `*Packet` (from `listener.go`, already in package), `packetPool` (already in package, `listener.go`).
- Produces: `slotHandle{streamID int; inbound chan *Packet; up chan struct{}}`, `dispatcher` with `newDispatcher()`, `setSlots(slots []*slotHandle, keepActiveStreamID int)`, `activeStreamID() int`, `currentSlots() []*slotHandle`, `rotateManual()`, `route(pkt *Packet)`, `run(ctx, inboundChan <-chan *Packet, rotateCh <-chan struct{})`. Consumed by `sessionManager` (Task 7/8) and `Run()` (Task 9).

- [ ] **Step 1: Write the failing tests**

```go
package udprelay

import (
	"context"
	"testing"
	"time"
)

func newTestSlot(streamID int) *slotHandle {
	return &slotHandle{
		streamID: streamID,
		inbound:  make(chan *Packet, slotInboundBufferSize),
		up:       make(chan struct{}, 1),
	}
}

func TestDispatcherRoutesToActiveSlot(t *testing.T) {
	t.Parallel()
	d := newDispatcher()
	s1, s2 := newTestSlot(1), newTestSlot(2)
	d.setSlots([]*slotHandle{s1, s2}, 1)

	pkt := &Packet{Data: []byte("x"), N: 1}
	d.route(pkt)

	select {
	case got := <-s1.inbound:
		if got != pkt {
			t.Fatal("wrong packet delivered to active slot")
		}
	default:
		t.Fatal("expected packet on active slot s1")
	}
	select {
	case <-s2.inbound:
		t.Fatal("inactive slot s2 must not receive packets")
	default:
	}
}

func TestDispatcherManualRotate(t *testing.T) {
	t.Parallel()
	d := newDispatcher()
	s1, s2 := newTestSlot(1), newTestSlot(2)
	d.setSlots([]*slotHandle{s1, s2}, 1)

	d.rotateManual()
	if got := d.activeStreamID(); got != 2 {
		t.Fatalf("expected active streamID 2 after manual rotate, got %d", got)
	}

	d.route(&Packet{Data: []byte("x"), N: 1})
	select {
	case <-s2.inbound:
	default:
		t.Fatal("expected packet on newly active slot s2")
	}
}

func TestDispatcherRotatesByVolume(t *testing.T) {
	t.Parallel()
	d := newDispatcher()
	s1, s2 := newTestSlot(1), newTestSlot(2)
	d.setSlots([]*slotHandle{s1, s2}, 1)

	// Один пакет с N=rotateThresholdBytes уже должен вызвать ротацию.
	d.route(&Packet{Data: make([]byte, 1), N: rotateThresholdBytes})

	if got := d.activeStreamID(); got != 2 {
		t.Fatalf("expected rotation to streamID 2 after crossing volume threshold, got %d", got)
	}
}

func TestDispatcherRunReadsInboundAndRotate(t *testing.T) {
	t.Parallel()
	d := newDispatcher()
	s1, s2 := newTestSlot(1), newTestSlot(2)
	d.setSlots([]*slotHandle{s1, s2}, 1)

	inboundChan := make(chan *Packet, 1)
	rotateCh := make(chan struct{}, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		d.run(ctx, inboundChan, rotateCh)
	}()

	rotateCh <- struct{}{}
	deadline := time.After(time.Second)
	for d.activeStreamID() != 2 {
		select {
		case <-deadline:
			t.Fatal("timed out waiting for run() to process rotateCh")
		case <-time.After(time.Millisecond):
		}
	}

	cancel()
	<-done
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `docker run --rm -v "$(pwd)":/src -w /src golang:1.26.5 go test ./internal/proxy/udprelay/... -run TestDispatcher -v`
Expected: FAIL — `undefined: slotHandle` / `undefined: newDispatcher`

- [ ] **Step 3: Write the implementation**

```go
package udprelay

import (
	"context"
	"sync"
	"time"
)

// slotHandle is what the dispatcher needs to route packets to, and observe
// the liveness of, one hot-set member. In production its fields are the
// SAME channels passed as DTLSLoop's inboundChan/okchan arguments (see
// sessionManager.launchSlot in sessionmgr.go), so wiring a slotHandle in
// doesn't change oneDTLS/oneTURN's behavior at all - the dispatcher is just
// a new writer/reader sitting between the listener and the existing
// per-stream loop. Tests build slotHandles directly and never launch
// DTLSLoop, exercising dispatcher logic with no network at all.
type slotHandle struct {
	streamID int
	inbound  chan *Packet  // dispatcher writes; DTLSLoop's write-goroutine reads it as its inboundChan
	up       chan struct{} // DTLSLoop signals here on every successful handshake, including reconnects (its okchan)
}

// slotInboundBufferSize - маленький буфер на слот, НАМЕРЕННО мал: живой
// слот (oneDTLS вычитывает inboundChan почти со скоростью сети) держит его
// почти всегда пустым, а мёртвый (между reconnect-попытками DTLSLoop, до
// 10-30s backoff) заполняет его за пару пакетов - переполнение служит
// дешёвым сигналом "слот сейчас не читает", см. maxConsecutiveDropsBeforeFailover.
const slotInboundBufferSize = 4

// rotateThresholdBytes - сколько байт пропустить через активный слот до
// плановой ротации на следующего кандидата hot-set'а. НЕ откалибровано
// живым замером - стартовая точка по спеке (см. "Открытые вопросы" в
// docs/superpowers/specs/2026-08-23-udp-relay-session-affinity-design.md),
// требует эмпирической калибровки отдельным прогоном.
const rotateThresholdBytes = 2 * 1024 * 1024

// maxConsecutiveDropsBeforeFailover - сколько подряд неудачных попыток
// отдать пакет активному слоту считать его мёртвым и переключаться
// немедленно, не дожидаясь плановой ротации по объёму.
const maxConsecutiveDropsBeforeFailover = 5

// dispatcher - единственный читатель общего inboundChan (см. run.go).
// Владеет индексом активного слота hot-set'а, переключает его по
// накопленному объёму, по ручному триггеру и по liveness-отказу активного
// слота. Не открывает и не закрывает сами TURN/DTLS-сессии - этим
// занимается sessionManager; dispatcher только маршрутизирует пакеты уже
// поднятых слотов.
type dispatcher struct {
	mu     sync.Mutex
	slots  []*slotHandle
	active int

	bytesSinceRotate uint64
	consecutiveDrops int
	lastUp           map[int]time.Time // streamID -> время последнего up-сигнала
}

func newDispatcher() *dispatcher {
	return &dispatcher{lastUp: make(map[int]time.Time)}
}

// setSlots (пере)задаёт состав hot-set'а. keepActiveStreamID - какой слот
// должен стать активным (первый слот из slots, если его там нет, например
// он и есть только что подключённый первый). Вызывается sessionManager'ом
// при старте и при каждой замене одного члена на refresh-тике.
func (d *dispatcher) setSlots(slots []*slotHandle, keepActiveStreamID int) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.slots = slots
	d.active = 0
	for i, s := range slots {
		if s.streamID == keepActiveStreamID {
			d.active = i
			break
		}
	}
	d.bytesSinceRotate = 0
	d.consecutiveDrops = 0
}

// currentSlots возвращает копию текущего состава hot-set'а - для
// sessionManager.refreshOne, чтобы решать, кого заменить, не держа мьютекс
// диспетчера дольше одного вызова.
func (d *dispatcher) currentSlots() []*slotHandle {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := make([]*slotHandle, len(d.slots))
	copy(out, d.slots)
	return out
}

// activeStreamID возвращает streamID текущего активного слота, 0 если
// hot-set пуст.
func (d *dispatcher) activeStreamID() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	if len(d.slots) == 0 {
		return 0
	}
	return d.slots[d.active].streamID
}

// markUp записывает момент последнего успешного handshake слота streamID.
// Вызывается отдельной горутиной-наблюдателем на каждый слот (см.
// sessionManager.launchSlot) - фан-ин через мьютекс вместо динамического
// select по растущему/убывающему числу каналов (состав hot-set'а меняется
// во время refresh).
func (d *dispatcher) markUp(streamID int, at time.Time) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.lastUp[streamID] = at
}

// rotateManual переключает активный слот немедленно, в обход порога по
// объёму - вызывается из run() при получении сигнала на rotateCh.
func (d *dispatcher) rotateManual() {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.rotateLocked()
}

// rotateLocked переключает активный индекс на следующий слот по кругу.
// Вызывающий обязан держать d.mu.
func (d *dispatcher) rotateLocked() {
	if len(d.slots) == 0 {
		return
	}
	d.active = (d.active + 1) % len(d.slots)
	d.bytesSinceRotate = 0
	d.consecutiveDrops = 0
}

// route отдаёт один пакет активному слоту, считает байты для плановой
// ротации и отслеживает подряд идущие отказы для liveness-failover.
func (d *dispatcher) route(pkt *Packet) {
	d.mu.Lock()
	if len(d.slots) == 0 {
		d.mu.Unlock()
		packetPool.Put(pkt)
		return
	}
	active := d.slots[d.active]
	d.mu.Unlock()

	select {
	case active.inbound <- pkt:
		d.mu.Lock()
		d.consecutiveDrops = 0
		d.bytesSinceRotate += uint64(pkt.N)
		if d.bytesSinceRotate >= rotateThresholdBytes {
			d.rotateLocked()
		}
		d.mu.Unlock()
	default:
		packetPool.Put(pkt)
		d.mu.Lock()
		d.consecutiveDrops++
		if d.consecutiveDrops >= maxConsecutiveDropsBeforeFailover {
			d.failoverLocked()
		}
		d.mu.Unlock()
	}
}

// run - главный цикл диспетчера: единственный читатель inboundChan,
// опционально слушает rotateCh на ручной триггер (nil rotateCh блокируется
// навсегда в select - безопасно, TCP+bond им не пользуется). Возвращается
// при отмене ctx.
func (d *dispatcher) run(ctx context.Context, inboundChan <-chan *Packet, rotateCh <-chan struct{}) {
	for {
		select {
		case <-ctx.Done():
			return
		case pkt := <-inboundChan:
			d.route(pkt)
		case <-rotateCh:
			d.rotateManual()
		}
	}
}
```

Note: `failoverLocked` is referenced here but defined in Task 6 — this task's tests (`TestDispatcherRoutesToActiveSlot`, `TestDispatcherManualRotate`, `TestDispatcherRotatesByVolume`, `TestDispatcherRunReadsInboundAndRotate`) never trigger `maxConsecutiveDropsBeforeFailover` drops, so the package still builds and this task's tests pass before Task 6 adds the method — but `route`'s `default` branch won't compile without it. Write Tasks 5 and 6 as one commit if executing strictly file-by-file; if using subagent-per-task execution, tell the Task 6 implementer this method is a forward reference required for Task 5 to compile, not new scope.

- [ ] **Step 4: Run test to verify it passes**

Run: `docker run --rm -v "$(pwd)":/src -w /src golang:1.26.5 go test ./internal/proxy/udprelay/... -run TestDispatcher -v`
Expected: PASS for all four tests once `failoverLocked` (Task 6) exists in the same package

- [ ] **Step 5: Commit**

```bash
git add internal/proxy/udprelay/dispatcher.go internal/proxy/udprelay/dispatcher_test.go
git commit -m "feat(udprelay): dispatcher core routing with manual and volume-based rotation"
```

---

### Task 6: dispatcher liveness failover

**Files:**
- Modify: `internal/proxy/udprelay/dispatcher.go`
- Modify: `internal/proxy/udprelay/dispatcher_test.go`

**Interfaces:**
- Produces: `dispatcher.failoverLocked()` (private, called from `route`'s drop path — already referenced in Task 5).

- [ ] **Step 1: Write the failing test**

Append to `internal/proxy/udprelay/dispatcher_test.go` (add `"time"` to imports if not already there from Task 5's `TestDispatcherRunReadsInboundAndRotate`):

```go
func TestDispatcherFailoverPicksMostRecentlyUpSlot(t *testing.T) {
	t.Parallel()
	d := newDispatcher()
	dead := &slotHandle{streamID: 1, inbound: make(chan *Packet)} // unbuffered: route() always hits default, nobody reads
	stale := newTestSlot(2)
	fresh := newTestSlot(3)
	d.setSlots([]*slotHandle{dead, stale, fresh}, 1)

	d.markUp(2, time.Now().Add(-time.Minute))
	d.markUp(3, time.Now())

	for i := 0; i < maxConsecutiveDropsBeforeFailover; i++ {
		d.route(&Packet{Data: []byte("x"), N: 1})
	}

	if got := d.activeStreamID(); got != 3 {
		t.Fatalf("expected failover to most recently up slot (3), got %d", got)
	}
}

func TestDispatcherFailoverFallsBackToNextWhenNoUpSignalKnown(t *testing.T) {
	t.Parallel()
	d := newDispatcher()
	dead := &slotHandle{streamID: 1, inbound: make(chan *Packet)}
	other := newTestSlot(2)
	d.setSlots([]*slotHandle{dead, other}, 1)
	// Ни один markUp не вызывался - failoverLocked не должен паниковать
	// и должен просто уйти по кругу.

	for i := 0; i < maxConsecutiveDropsBeforeFailover; i++ {
		d.route(&Packet{Data: []byte("x"), N: 1})
	}

	if got := d.activeStreamID(); got != 2 {
		t.Fatalf("expected fallback round-robin to streamID 2, got %d", got)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `docker run --rm -v "$(pwd)":/src -w /src golang:1.26.5 go test ./internal/proxy/udprelay/... -run TestDispatcherFailover -v`
Expected: FAIL — `undefined: d.failoverLocked` (compile error, since Task 5 only references it)

- [ ] **Step 3: Write the implementation**

Append to `internal/proxy/udprelay/dispatcher.go`:

```go
// failoverLocked switches the active slot to the most recently up-signaled
// among the OTHERS (never the one that just failed) - the best available
// proxy for "pick a live one" without a separate down-signal (oneDTLS/
// DTLSLoop don't expose one; adding it would mean changing their internals,
// which this feature deliberately avoids - see loop.go's doc comment and
// the spec's "no changes inside them" constraint). Falls back to plain
// round-robin if no other slot has ever signaled up yet. Caller must hold d.mu.
func (d *dispatcher) failoverLocked() {
	if len(d.slots) <= 1 {
		d.bytesSinceRotate = 0
		d.consecutiveDrops = 0
		return
	}
	deadID := d.slots[d.active].streamID
	best := -1
	var bestTime time.Time
	for i, s := range d.slots {
		if s.streamID == deadID {
			continue
		}
		if t := d.lastUp[s.streamID]; t.After(bestTime) {
			bestTime = t
			best = i
		}
	}
	if best >= 0 {
		d.active = best
	} else {
		d.active = (d.active + 1) % len(d.slots)
	}
	d.bytesSinceRotate = 0
	d.consecutiveDrops = 0
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `docker run --rm -v "$(pwd)":/src -w /src golang:1.26.5 go test ./internal/proxy/udprelay/... -v`
Expected: PASS — every test in the package, Tasks 1 through 6 combined

- [ ] **Step 5: Commit**

```bash
git add internal/proxy/udprelay/dispatcher.go internal/proxy/udprelay/dispatcher_test.go
git commit -m "feat(udprelay): dispatcher liveness failover on active-slot send failure"
```

---

### Task 7: `sessionManager` — initial hot-set launch with startup barrier

**Files:**
- Create: `internal/proxy/udprelay/sessionmgr.go`

**Interfaces:**
- Consumes: `slotHandle`, `dispatcher` (Task 5/6), `groupPrefix24` (Task 1), `Deps`, `Params`, `DTLSLoop`, `TURNLoop`, `streamStartBarrier` (all already in package, `run.go`/`loop.go`).
- Produces: `newSessionManager(deps *Deps, params *Params, peer *net.UDPAddr, listenConn net.PacketConn, k int, t <-chan time.Time) *sessionManager`, `(sm *sessionManager) onAllocated(streamID int, addr *net.UDPAddr)`, `(sm *sessionManager) launchSlot(ctx context.Context, wg *sync.WaitGroup, streamID int, barrierCh chan<- struct{}) *slotHandle`. Consumed by `Run()` (Task 9) and by Task 8's `refreshOne`.

No test — see "Global note on test coverage": this launches real `DTLSLoop`/`TURNLoop` goroutines against a real network, same as `Run()` always has.

- [ ] **Step 1: Write `sessionManager` and `launchSlot`**

```go
package udprelay

import (
	"context"
	"net"
	"sync"
	"time"
)

// sessionManager владеет hot-set'ом (K живых DTLS+TURN слотов) и
// dispatcher'ом, который маршрутизирует в него пакеты. Заменяет прежний
// цикл `for i := range numStreams { go maintainSession... }` в Run():
// вместо N параллельных независимых стримов держит K << N живых, ротируя
// активный через dispatcher и периодически обновляя состав (см.
// refreshOne, sessionmgr.go продолжение в Task 8). Переиспользует
// DTLSLoop/TURNLoop без изменений - каждому слоту достаётся собственный
// маленький inbound-канал вместо общего inboundChan, который теперь
// единолично читает dispatcher.
type sessionManager struct {
	deps       *Deps
	params     *Params
	peer       *net.UDPAddr
	listenConn net.PacketConn
	k          int
	t          <-chan time.Time // общий тик TURNLoop, один на процесс, как и раньше

	disp *dispatcher

	nextID int // следующий свободный streamID; трогает только run()'s горутина

	mu      sync.Mutex // защищает только groups - onAllocated зовётся из per-slot горутин
	groups  map[int]string
	cancels map[int]context.CancelFunc // трогает только run()'s горутина (launchSlot/refreshOne)
}

func newSessionManager(deps *Deps, params *Params, peer *net.UDPAddr, listenConn net.PacketConn, k int, t <-chan time.Time) *sessionManager {
	if k <= 0 {
		k = 1
	}
	return &sessionManager{
		deps:       deps,
		params:     params,
		peer:       peer,
		listenConn: listenConn,
		k:          k,
		t:          t,
		disp:       newDispatcher(),
		groups:     make(map[int]string),
		cancels:    make(map[int]context.CancelFunc),
	}
}

// onAllocated записывается в sm.params.OnAllocated до запуска первого
// слота - общий на весь hot-set, различает слоты по streamID. Вызывается
// из oneTURN, т.е. из per-slot горутины, конкурентно с остальными - отсюда
// мьютекс на groups (в отличие от nextID/cancels, которые трогает только
// горутина run()).
func (sm *sessionManager) onAllocated(streamID int, addr *net.UDPAddr) {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	sm.groups[streamID] = groupPrefix24(addr.String())
}

// launchSlot поднимает DTLSLoop/TURNLoop для нового streamID и заводит
// slotHandle для диспетчера - ТЕ ЖЕ функции, что Run() запускал раньше для
// каждого из N стримов, просто с собственным маленьким inbound-каналом
// вместо общего. barrierCh, если не nil, получает один сигнал при первом
// успешном handshake этого слота - используется только для стартового
// барьера первого слота hot-set'а (см. run(), тот же барьер, что раньше
// был в Run() для стрима 1). Каждое успешное (пере)подключение слота
// дополнительно отмечается в диспетчере через markUp для liveness-failover.
func (sm *sessionManager) launchSlot(ctx context.Context, wg *sync.WaitGroup, streamID int, barrierCh chan<- struct{}) *slotHandle {
	slotCtx, cancel := context.WithCancel(ctx)
	sm.cancels[streamID] = cancel

	slot := &slotHandle{
		streamID: streamID,
		inbound:  make(chan *Packet, slotInboundBufferSize),
		up:       make(chan struct{}, 1),
	}

	cchan := make(chan net.PacketConn)
	wg.Add(1)
	go func() {
		defer wg.Done()
		DTLSLoop(slotCtx, sm.deps, sm.params, sm.peer, sm.listenConn, slot.inbound, cchan, slot.up, streamID)
	}()
	wg.Add(1)
	go func() {
		defer wg.Done()
		TURNLoop(slotCtx, sm.deps, sm.params, sm.peer, cchan, sm.t, streamID)
	}()
	wg.Add(1)
	go func() {
		defer wg.Done()
		first := true
		for {
			select {
			case <-slotCtx.Done():
				return
			case <-slot.up:
				sm.disp.markUp(streamID, time.Now())
				if first && barrierCh != nil {
					select {
					case barrierCh <- struct{}{}:
					default:
					}
				}
				first = false
			}
		}
	}()

	return slot
}
```

- [ ] **Step 2: Write `run`'s startup half (barrier + initial hot-set)**

Append to `internal/proxy/udprelay/sessionmgr.go`:

```go
// run запускает начальный hot-set: первый слот один, с барьером прогрева
// кэша credentials (идентично тому, как раньше Run() ждал стрим 1 перед
// запуском 2..N), затем оставшиеся K-1 сразу следом. После этого ведёт
// dispatcher и refresh-цикл (см. Task 8's refreshOne) до отмены ctx.
func (sm *sessionManager) run(ctx context.Context, inboundChan <-chan *Packet, rotateCh <-chan struct{}) {
	wg := sync.WaitGroup{}
	sm.nextID = 1

	barrierCh := make(chan struct{}, 1)
	first := sm.launchSlot(ctx, &wg, 1, barrierCh)

	select {
	case <-barrierCh:
	case <-ctx.Done():
	case <-time.After(streamStartBarrier):
	}

	slots := []*slotHandle{first}
	for i := 1; i < sm.k; i++ {
		sm.nextID++
		slots = append(slots, sm.launchSlot(ctx, &wg, sm.nextID, nil))
	}
	sm.disp.setSlots(slots, first.streamID)

	dispDone := make(chan struct{})
	go func() {
		defer close(dispDone)
		sm.disp.run(ctx, inboundChan, rotateCh)
	}()

	sm.refreshLoop(ctx, &wg)

	wg.Wait()
	<-dispDone
}
```

Note: `refreshLoop` is written in Task 8 — until then this file doesn't compile standalone. Same forward-reference situation as Task 5/6; if executing task-by-task with a fresh reviewer per task, land Tasks 7 and 8 as one review unit, or note explicitly that Task 7's `run` calls a Task 8 method.

- [ ] **Step 3: Build to confirm the package compiles once Task 8 lands**

Run (after Task 8 is also in place): `docker run --rm -v "$(pwd)":/src -w /src golang:1.26.5 go build ./internal/proxy/udprelay/...`
Expected: no output (clean build)

- [ ] **Step 4: Commit**

```bash
git add internal/proxy/udprelay/sessionmgr.go
git commit -m "feat(udprelay): sessionManager initial hot-set launch with startup barrier"
```

---

### Task 8: `sessionManager.refreshOne` — periodic hot-set membership refresh

**Files:**
- Modify: `internal/proxy/udprelay/sessionmgr.go`

**Interfaces:**
- Consumes: `pickReplacementCandidate`, `shouldRefresh` (Task 2/3).
- Produces: `(sm *sessionManager) refreshLoop(ctx context.Context, wg *sync.WaitGroup)`, `(sm *sessionManager) refreshOne(ctx context.Context, wg *sync.WaitGroup)` — completes `run`'s call from Task 7.

No test — same reasoning as Task 7 (launches real goroutines).

- [ ] **Step 1: Write `refreshOne` and `refreshLoop`**

Append to `internal/proxy/udprelay/sessionmgr.go` (add `"internal/logx"` is not needed; only stdlib already imported):

```go
// hotSetRefreshInterval - как часто sessionManager рассматривает замену
// одного неактивного члена hot-set'а на свежего кандидата. НЕ
// откалибровано живым замером - стартовая точка по спеке, требует
// эмпирической калибровки (см. dispatcher.go's rotateThresholdBytes comment
// for the same caveat).
const hotSetRefreshInterval = 5 * time.Minute

// refreshLoop periodically calls refreshOne while ctx is alive. Runs on the
// same goroutine as run() (called at the end of it, see Task 7) rather than
// its own - nextID and cancels are only ever touched from here or from
// launchSlot, which this goroutine also calls, so neither field needs its
// own lock (see the sessionManager doc comment).
func (sm *sessionManager) refreshLoop(ctx context.Context, wg *sync.WaitGroup) {
	ticker := time.NewTicker(hotSetRefreshInterval)
	defer ticker.Stop()
	lastRefresh := time.Now()

	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			if shouldRefresh(lastRefresh, now, hotSetRefreshInterval) {
				sm.refreshOne(ctx, wg)
				lastRefresh = now
			}
		}
	}
}

// refreshOne retires one non-active, group-over-represented hot-set member
// (see pickReplacementCandidate) and launches a fresh candidate (next
// sequential streamID) in its place. A no-op if nothing is clearly
// over-represented right now (including while some members' groups are
// still unknown - see Params.OnAllocated) - degrades to no churn, never to
// an error.
func (sm *sessionManager) refreshOne(ctx context.Context, wg *sync.WaitGroup) {
	slots := sm.disp.currentSlots()
	if len(slots) == 0 {
		return
	}
	ids := make([]int, len(slots))
	for i, s := range slots {
		ids[i] = s.streamID
	}
	active := sm.disp.activeStreamID()

	sm.mu.Lock()
	groupsCopy := make(map[int]string, len(sm.groups))
	for k, v := range sm.groups {
		groupsCopy[k] = v
	}
	sm.mu.Unlock()

	retireID := pickReplacementCandidate(ids, active, groupsCopy)
	if retireID < 0 {
		return
	}

	sm.nextID++
	fresh := sm.launchSlot(ctx, wg, sm.nextID, nil)

	newSlots := make([]*slotHandle, 0, len(slots))
	for _, s := range slots {
		if s.streamID == retireID {
			continue
		}
		newSlots = append(newSlots, s)
	}
	newSlots = append(newSlots, fresh)
	sm.disp.setSlots(newSlots, active)

	if cancel, ok := sm.cancels[retireID]; ok {
		cancel()
		delete(sm.cancels, retireID)
	}
	sm.mu.Lock()
	delete(sm.groups, retireID)
	sm.mu.Unlock()
}
```

- [ ] **Step 2: Build the whole package**

Run: `docker run --rm -v "$(pwd)":/src -w /src golang:1.26.5 go build ./internal/proxy/udprelay/... && go vet ./internal/proxy/udprelay/...`
Expected: clean (no output)

- [ ] **Step 3: Run the full package test suite**

Run: `docker run --rm -v "$(pwd)":/src -w /src golang:1.26.5 go test ./internal/proxy/udprelay/... -v`
Expected: PASS — all tests from Tasks 1, 2, 3, 5, 6

- [ ] **Step 4: Commit**

```bash
git add internal/proxy/udprelay/sessionmgr.go
git commit -m "feat(udprelay): sessionManager periodic hot-set membership refresh"
```

---

### Task 9: Wire `sessionManager` into `Run()`

**Files:**
- Modify: `internal/proxy/udprelay/run.go`

**Interfaces:**
- Consumes: `newSessionManager`, `sessionManager.run`, `sessionManager.onAllocated` (Tasks 7/8).
- Produces: `Run`'s exported signature changes its last parameter's name and meaning: `numStreams int` (total independent streams) → `hotSetK int` (hot-set size). Same position, same type - callers (Task 10/11) must update what they pass, not how they call.

- [ ] **Step 1: Replace the per-stream loop**

In `internal/proxy/udprelay/run.go`, change the `Run` signature:

```go
// Run - точка входа UDP-режима. Биндит listenAddr, распределяет входящие
// пакеты через dispatcher в hot-set из hotSetK живых DTLS+TURN сессий
// (вместо прежних N независимых, см. docs/superpowers/specs/2026-08-23-udp-relay-session-affinity-design.md).
// connectedStreams принадлежит вызывающему (provider может читать через
// свой StreamsAlive-аналог) и инкрементируется/декрементируется в oneTURN,
// как и раньше. Возвращается после выхода всех потоков (т.е. при отмене
// ctx). При фатальной provider-ошибке возвращает ErrFatal - вызывающий
// делает os.Exit без вмешательства udprelay в хост-процесс.
func Run(ctx context.Context, dtlsDialer *dtlsdial.Dialer, auth AuthHandler, logger logx.Logger, connectedStreams *atomic.Int32, params *Params, peer *net.UDPAddr, listenAddr string, hotSetK int) error {
```

And replace the body from `if numStreams <= 0 { numStreams = 1 }` down through the `for i := 1; i < numStreams; i++ { ... }` block (currently lines ~99-161) with:

```go
	if hotSetK <= 0 {
		hotSetK = 1
	}

	fatalCh := make(chan error, 1)
	var activeLocalPeer atomic.Value
	deps := &Deps{
		DTLSDialer:       dtlsDialer,
		Auth:             auth,
		Log:              logger,
		ActiveLocalPeer:  &activeLocalPeer,
		ConnectedStreams: connectedStreams,
		fatalCh:          fatalCh,
	}

	runCtx, runCancel := context.WithCancel(ctx)
	defer runCancel()

	inboundChan := make(chan *Packet, inboundQueueCap)
	wg := sync.WaitGroup{}
	wg.Go(func() {
		runListener(runCtx, listenConn, &activeLocalPeer, inboundChan)
	})
	t := time.Tick(200 * time.Millisecond)

	sm := newSessionManager(deps, params, peer, listenConn, hotSetK, t)
	params.OnAllocated = sm.onAllocated
	wg.Go(func() {
		sm.run(runCtx, inboundChan, params.RotateCh)
	})
```

(The rest of `Run` — the `fatalErr`/`watcherDone` block and final `wg.Wait(); runCancel(); ...; return` — is unchanged; `wg` still tracks exactly one more goroutine now, `sm.run`, instead of the old `2*numStreams`.)

- [ ] **Step 2: Build**

Run: `docker run --rm -v "$(pwd)":/src -w /src golang:1.26.5 go build ./internal/proxy/udprelay/...`
Expected: FAILS at this point — `cmd/client/main.go` and `mobile/mobile.go` still call the old signature; that's Tasks 10/11. Confirm the failure is only in those two packages, not in `udprelay` itself:

Run: `docker run --rm -v "$(pwd)":/src -w /src golang:1.26.5 go vet ./internal/proxy/udprelay/...`
Expected: clean (the package itself is internally consistent; only its external callers are now stale)

- [ ] **Step 3: Commit**

```bash
git add internal/proxy/udprelay/run.go
git commit -m "feat(udprelay): wire sessionManager into Run, numStreams becomes hotSetK"
```

(This commit intentionally leaves `cmd/client` and `mobile` non-building — Tasks 10/11 fix both in the same session; do not push between these commits.)

---

### Task 10: `cmd/client/main.go` — stdin `rotate` command + call-site update

**Files:**
- Modify: `cmd/client/main.go`

**Interfaces:**
- Consumes: `udprelay.Run`'s new signature (Task 9), `udprelay.Params.RotateCh` (Task 4).

- [ ] **Step 1: Add the stdin reader function**

Add near the other helper functions in `cmd/client/main.go` (e.g. next to `logTrafficStats`):

```go
// readRotateCommands читает построчные команды из stdin запущенного
// процесса - Android (CoreProcessController) и cmd/desktop уже держат
// клиент как подпроцесс с открытым stdin/stdout, симметрично добавляем
// чтение команд туда же, без нового порта/listener'а (см.
// docs/superpowers/specs/2026-08-23-udp-relay-session-affinity-design.md,
// "Failover"). Сейчас поддерживает только "rotate" - ручное переключение
// активного слота hot-set'а в -transport udp.
func readRotateCommands(ctx context.Context, logger logx.Logger, rotateCh chan<- struct{}) {
	scanner := bufio.NewScanner(os.Stdin)
	for scanner.Scan() {
		if ctx.Err() != nil {
			return
		}
		if strings.TrimSpace(scanner.Text()) != "rotate" {
			continue
		}
		select {
		case rotateCh <- struct{}{}:
		default:
		}
		logger.Infof("[rotate] manual hot-set rotation requested")
	}
}
```

Add `"bufio"` and `"strings"` to the import block if not already present (check first — `os` is already imported for `os.Exit`).

- [ ] **Step 2: Wire it into the UDP branch and update the `Run` call**

In `main()`, in the UDP branch (after the early `return` for the TCP branch, before `udpParams := &udprelay.Params{...}`):

```go
	rotateCh := make(chan struct{}, 1)
	go readRotateCommands(ctx, logger, rotateCh)

	udpDtlsDialer := &dtlsdial.Dialer{
		HandshakeTimeout: 20 * time.Second,
		HandshakeSem:     make(chan struct{}, dtlsHandshakeConcurrency),
	}
	udpParams := &udprelay.Params{
		Host:         cfg.TURN.Host,
		Port:         cfg.TURN.Port,
		TransportUDP: cfg.TURN.TransportUDP,
		Profile:      string(cfg.Obf.Profile),
		ObfKey:       cfg.Obf.Key,
		ObfTiming:    cfg.Obf.Timing,
		GetCreds:     udprelay.GetCredsFunc(getCreds),
		ClientID:     cfg.ClientID,
		TrafficStats: trafficStats,
		RotateCh:     rotateCh,
	}
	if err := udprelay.Run(ctx, udpDtlsDialer, prov, logger, &connectedStreams, udpParams, peer, cfg.Proxy.Listen, cfg.TURN.N); err != nil {
```

(Only the last argument changes, from `totalStreams` to `cfg.TURN.N` — the TCP branch above it, using `tcpfwd.Run(..., totalStreams, ...)`, is untouched; `totalStreams` is still needed there.)

- [ ] **Step 3: Build**

Run: `docker run --rm -v "$(pwd)":/src -w /src golang:1.26.5 go build ./cmd/client/...`
Expected: clean build

- [ ] **Step 4: Commit**

```bash
git add cmd/client/main.go
git commit -m "feat(client): stdin 'rotate' command, -n becomes UDP hot-set size"
```

---

### Task 11: `mobile/mobile.go` — `TriggerRotate()` + call-site update

**Files:**
- Modify: `mobile/mobile.go`

**Interfaces:**
- Produces: exported `func TriggerRotate()` — the gomobile-bind entry point iOS calls in-process (no stdin available there, unlike Android/desktop's subprocess model).
- Consumes: `udprelay.Run`'s new signature (Task 9), `udprelay.Params.RotateCh` (Task 4).

- [ ] **Step 1: Add package-level rotate-channel state and `TriggerRotate`**

Add near the other package-level state in `mobile/mobile.go` (e.g. next to `statusVal`):

```go
var activeRotateCh atomic.Pointer[chan struct{}]

// TriggerRotate запрашивает немедленное переключение активного слота
// hot-set'а в текущей UDP-сессии (см.
// docs/superpowers/specs/2026-08-23-udp-relay-session-affinity-design.md,
// "Failover"). Ручной триггер для iOS: там нет отдельного
// stdin-подпроцесса как у Android/desktop (см. cmd/client/main.go's
// readRotateCommands), gomobile зовёт эту функцию in-process напрямую.
// No-op, если сессия не запущена либо запущена не в UDP-режиме.
func TriggerRotate() {
	p := activeRotateCh.Load()
	if p == nil {
		return
	}
	select {
	case *p <- struct{}{}:
	default:
	}
}
```

- [ ] **Step 2: Wire it into the UDP branch and update the `Run` call**

In `startWithArgs`'s UDP branch (mirroring Task 10's placement in `cmd/client/main.go`), before `udpParams := &udprelay.Params{...}`:

```go
			rotateCh := make(chan struct{}, 1)
			activeRotateCh.Store(&rotateCh)
			defer activeRotateCh.CompareAndSwap(&rotateCh, nil)

			udpDtlsDialer := &dtlsdial.Dialer{
				HandshakeTimeout: 20 * time.Second,
				HandshakeSem:     make(chan struct{}, 3),
			}
			udpParams := &udprelay.Params{
				Host:         cfg.TURN.Host,
				Port:         cfg.TURN.Port,
				TransportUDP: cfg.TURN.TransportUDP,
				Profile:      string(cfg.Obf.Profile),
				ObfKey:       cfg.Obf.Key,
				ObfTiming:    cfg.Obf.Timing,
				GetCreds:     udprelay.GetCredsFunc(getCreds),
				ClientID:     cfg.ClientID,
				TrafficStats: traffic.stats,
				RotateCh:     rotateCh,
			}

			if err := udprelay.Run(ctx, udpDtlsDialer, prov, logger, &connectedStreams, udpParams, peerAddr, cfg.Proxy.Listen, cfg.TURN.N); err != nil {
```

`activeRotateCh.CompareAndSwap(&rotateCh, nil)` only clears the pointer if it's still THIS session's channel — matches the existing `sessionGen` multi-start-safety convention already in this file (a newer `Start()` racing a slow `Stop()` must not have its rotate channel clobbered by the older session's cleanup).

- [ ] **Step 3: Build**

Run: `docker run --rm -v "$(pwd)":/src -w /src golang-ayatana:latest sh -c "git config --global --add safe.directory /src; go build -buildvcs=false ./..."`
Expected: `BUILD_OK` — this also confirms `cmd/desktop` (which doesn't touch `udprelay` directly) still links, and is the first point since Task 9 where the *entire* module builds again

- [ ] **Step 4: Run the full test suite**

Run: `docker run --rm -v "$(pwd)":/src -w /src golang-ayatana:latest sh -c "git config --global --add safe.directory /src; go test -buildvcs=false ./..."`
Expected: every package `ok` or `[no test files]`, none `FAIL`

- [ ] **Step 5: Commit**

```bash
git add mobile/mobile.go
git commit -m "feat(mobile): TriggerRotate() for iOS in-process manual hot-set rotation"
```

---

### Task 12: `docs/flags.md` — document `-n`'s new UDP-mode meaning

**Files:**
- Modify: `docs/flags.md`

- [ ] **Step 1: Add the note next to the existing `-n` row**

The current table row (line 14) reads:

```
| `-n` | `10` | параллельных TURN-потоков; это потолок, не обязательный минимум - каждый поток ретраит независимо (с backoff при квоте), туннель работает и с частью потоков, недостающие сами доедут по мере снятия квоты/восстановления сети |
```

Add a paragraph right after the flags table (before the next section heading):

```markdown
**`-n` в `-transport udp`:** с версии, включающей session affinity (см.
`docs/superpowers/specs/2026-08-23-udp-relay-session-affinity-design.md`),
`-n` в этом режиме означает не число параллельных TURN-потоков, а размер
"hot-set" - сколько сессий держать живыми одновременно, пока активной в
каждый момент времени является ровно одна (переключение по накопленному
объёму трафика, не по каждому пакету - так исправлен флаппинг WireGuard
endpoint'а при `-transport udp`). Практический эффект: то же число `-n`
теперь держит МЕНЬШЕ одновременных TURN-аллокаций, чем раньше. Семейные
профили (`-n 20/30/40`) подобраны под старую семантику "N независимых
потоков" - пересмотреть при следующем батч-релизе клиентов, не разово. В
`-transport tcp`/`tcpfwd`-режимах `-n` не меняет смысл.
```

- [ ] **Step 2: Commit**

```bash
git add docs/flags.md
git commit -m "docs: -n means UDP hot-set size now, not stream count"
```

---

## After all tasks: full verification

```bash
docker run --rm -v "$(pwd)":/src -w /src golang-ayatana:latest sh -c "
  git config --global --add safe.directory /src
  gofmt -l .
  go build -buildvcs=false ./...
  go vet -buildvcs=false ./...
  go test -buildvcs=false ./...
"
```
Expected: `gofmt -l .` prints nothing, build/vet clean, every package `ok` or `[no test files]`.

This gets the feature building, internally consistent, and unit-tested wherever the codebase's own conventions call for a unit test. It does **not** validate that the WireGuard-flapping fix actually works live — that needs the same before/after measurement already queued from the KCP/DTLS tuning work (`ssh vps "wg show wgcl dump"` sampled repeatedly, comparing endpoint-change frequency and throughput pre/post), and it does **not** calibrate `rotateThresholdBytes` or `hotSetRefreshInterval` — both are explicitly flagged in this plan as needing that same live measurement pass before being trusted as final values.
