package netconn

import (
	"net"
	"sync"
	"testing"
	"time"
)

// fakePacketConn is a minimal net.PacketConn test double recording WriteTo
// calls, in the same spirit as multisplit_test.go's recorder for net.Conn.
// entered/hold let a test synchronize with loop()'s single background
// goroutine deterministically (e.g. to prove a queue-full drop) without
// relying on sleeps.
type fakePacketConn struct {
	mu     sync.Mutex
	writes []fakeWrite
	closed bool

	entered chan struct{} // signaled (non-blocking send) on WriteTo entry, if set
	hold    chan struct{} // WriteTo blocks reading this, if set, until closed

	// closeCh mimics real net.Conn semantics: closing the connection
	// unblocks a Write that's currently stuck in the kernel (the runtime
	// poller wakes it with "use of closed network connection"). Without
	// this, a test that blocks WriteTo on hold and never closes hold
	// itself would have no way to simulate Close() being what unwedges
	// the stuck write.
	closeCh   chan struct{}
	closeOnce sync.Once
}

type fakeWrite struct {
	b    []byte
	addr net.Addr
}

func newFakePacketConn() *fakePacketConn {
	return &fakePacketConn{closeCh: make(chan struct{})}
}

func (f *fakePacketConn) WriteTo(p []byte, addr net.Addr) (int, error) {
	if f.entered != nil {
		select {
		case f.entered <- struct{}{}:
		default:
		}
	}
	if f.hold != nil {
		select {
		case <-f.hold:
		case <-f.closeCh:
			return 0, net.ErrClosed
		}
	}
	b := make([]byte, len(p))
	copy(b, p)
	f.mu.Lock()
	f.writes = append(f.writes, fakeWrite{b, addr})
	f.mu.Unlock()
	return len(p), nil
}

func (f *fakePacketConn) writeCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.writes)
}

func (f *fakePacketConn) ReadFrom(_ []byte) (int, net.Addr, error) {
	select {}
}
func (f *fakePacketConn) Close() error {
	f.mu.Lock()
	f.closed = true
	f.mu.Unlock()
	f.closeOnce.Do(func() { close(f.closeCh) })
	return nil
}
func (f *fakePacketConn) LocalAddr() net.Addr              { return nil }
func (f *fakePacketConn) SetDeadline(time.Time) error      { return nil }
func (f *fakePacketConn) SetReadDeadline(time.Time) error  { return nil }
func (f *fakePacketConn) SetWriteDeadline(time.Time) error { return nil }

// (1) WriteTo returns success and the packet reaches the underlying conn.
func TestNonBlockingPacketConn_WriteToReachesUnderlyingConn(t *testing.T) {
	t.Parallel()
	fake := newFakePacketConn()
	c := NewNonBlockingPacketConn(fake, 4)
	defer c.Close()

	addr := &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 1234}
	n, err := c.WriteTo([]byte("hello"), addr)
	if err != nil {
		t.Fatalf("WriteTo err: %v", err)
	}
	if n != 5 {
		t.Fatalf("WriteTo n=%d, want 5", n)
	}

	deadline := time.After(time.Second)
	for {
		if fake.writeCount() == 1 {
			break
		}
		select {
		case <-deadline:
			t.Fatal("timed out waiting for underlying WriteTo")
		case <-time.After(time.Millisecond):
		}
	}
	fake.mu.Lock()
	got := fake.writes[0]
	fake.mu.Unlock()
	if string(got.b) != "hello" {
		t.Fatalf("underlying write = %q, want %q", got.b, "hello")
	}
	if got.addr != addr {
		t.Fatalf("underlying addr = %v, want %v", got.addr, addr)
	}
}

// (2) queue-full drops the packet without error.
func TestNonBlockingPacketConn_QueueFullDropsSilently(t *testing.T) {
	t.Parallel()
	fake := &fakePacketConn{
		entered: make(chan struct{}, 1),
		hold:    make(chan struct{}),
		closeCh: make(chan struct{}),
	}
	c := NewNonBlockingPacketConn(fake, 1) // capacity 1

	addrA := &net.UDPAddr{Port: 1}
	addrB := &net.UDPAddr{Port: 2}
	addrC := &net.UDPAddr{Port: 3}

	// pA: loop() dequeues it immediately and blocks inside fake.WriteTo on
	// hold — this guarantees c.ch is now empty and loop() is stuck, so the
	// next sends are deterministic instead of racing the drain goroutine.
	if _, err := c.WriteTo([]byte("A"), addrA); err != nil {
		t.Fatalf("WriteTo A: %v", err)
	}
	select {
	case <-fake.entered:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for loop() to enter underlying WriteTo")
	}

	// pB: fills the 1-capacity queue (loop is stuck on pA).
	if _, err := c.WriteTo([]byte("B"), addrB); err != nil {
		t.Fatalf("WriteTo B: %v", err)
	}
	// pC: queue full — must drop, but still report success to the caller.
	n, err := c.WriteTo([]byte("C"), addrC)
	if err != nil {
		t.Fatalf("WriteTo C returned error, want silent drop: %v", err)
	}
	if n != 1 {
		t.Fatalf("WriteTo C n=%d, want 1 (len of dropped payload)", n)
	}

	// Release the drain and let it flush pA and pB.
	close(fake.hold)
	deadline := time.After(time.Second)
	for {
		if fake.writeCount() == 2 {
			break
		}
		select {
		case <-deadline:
			t.Fatalf("timed out waiting for drain, got %d writes", fake.writeCount())
		case <-time.After(time.Millisecond):
		}
	}
	c.Close()

	fake.mu.Lock()
	defer fake.mu.Unlock()
	if len(fake.writes) != 2 {
		t.Fatalf("underlying writes = %d, want 2 (A and B only, C dropped)", len(fake.writes))
	}
	if string(fake.writes[0].b) != "A" || string(fake.writes[1].b) != "B" {
		t.Fatalf("underlying writes = %q, %q, want A, B (C must not appear)", fake.writes[0].b, fake.writes[1].b)
	}
}

// (3) Close() actually causes loop() to exit.
func TestNonBlockingPacketConn_CloseStopsLoop(t *testing.T) {
	t.Parallel()
	fake := newFakePacketConn()
	c := NewNonBlockingPacketConn(fake, 4)

	if err := c.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	select {
	case <-c.stopped:
	case <-time.After(time.Second):
		t.Fatal("loop() did not exit within 1s of Close()")
	}

	fake.mu.Lock()
	closed := fake.closed
	fake.mu.Unlock()
	if !closed {
		t.Fatal("underlying conn was not closed")
	}

	if _, err := c.WriteTo([]byte("x"), &net.UDPAddr{}); err != net.ErrClosed {
		t.Fatalf("WriteTo after Close = %v, want net.ErrClosed", err)
	}
}

// (4) a packet queued immediately before Close() still reaches the
// underlying conn — proves the drain-on-close behavior turndial.go's
// delete-allocation Refresh depends on.
func TestNonBlockingPacketConn_CloseDrainsQueuedPacket(t *testing.T) {
	t.Parallel()
	fake := newFakePacketConn()
	c := NewNonBlockingPacketConn(fake, 4)

	addr := &net.UDPAddr{Port: 1}
	if _, err := c.WriteTo([]byte("bye"), addr); err != nil {
		t.Fatalf("WriteTo: %v", err)
	}
	if err := c.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	fake.mu.Lock()
	defer fake.mu.Unlock()
	if len(fake.writes) != 1 || string(fake.writes[0].b) != "bye" {
		t.Fatalf("underlying writes = %v, want [\"bye\"]", fake.writes)
	}
}

// Proves Close() doesn't hang forever when loop() is stuck inside a
// blocking underlying WriteTo (e.g. a blackholed TCP path with a full send
// buffer and no write deadline set anywhere in the chain) — without this,
// Close() would wait for the OS's full retry window (~15 minutes on
// Linux's default tcp_retries2), which defeats PermDead-triggered
// reconnect (tcpfwd's maintainSession calls Close synchronously before
// reconnecting with fresh credentials).
func TestNonBlockingPacketConn_CloseUnwedgesStuckWrite(t *testing.T) {
	t.Parallel()
	fake := &fakePacketConn{
		entered: make(chan struct{}, 1),
		hold:    make(chan struct{}), // never closed: simulates a permanently stuck write
		closeCh: make(chan struct{}),
	}
	c := NewNonBlockingPacketConn(fake, 4)

	if _, err := c.WriteTo([]byte("x"), &net.UDPAddr{Port: 1}); err != nil {
		t.Fatalf("WriteTo: %v", err)
	}
	select {
	case <-fake.entered:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for loop() to enter the stuck underlying WriteTo")
	}

	closed := make(chan error, 1)
	start := time.Now()
	go func() { closed <- c.Close() }()

	select {
	case err := <-closed:
		if err != nil {
			t.Fatalf("Close: %v", err)
		}
		if elapsed := time.Since(start); elapsed > 2*time.Second {
			t.Fatalf("Close took %v, want well under closeDrainTimeout+margin", elapsed)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Close() hung on a stuck underlying WriteTo — closeDrainTimeout did not unwedge it")
	}

	fake.mu.Lock()
	defer fake.mu.Unlock()
	if !fake.closed {
		t.Fatal("underlying conn was never closed to unwedge the stuck write")
	}
}

// Proves C2 is actually fixed: WriteTo called concurrently from many
// goroutines (pion does this — caller goroutine, retransmit timer,
// allocation-refresh ticker) must not race. Run with -race.
func TestNonBlockingPacketConn_ConcurrentWriteToNoRace(t *testing.T) {
	t.Parallel()
	fake := newFakePacketConn()
	c := NewNonBlockingPacketConn(fake, 256)

	const goroutines = 16
	const perGoroutine = 100
	var wg sync.WaitGroup
	wg.Add(goroutines)
	for g := 0; g < goroutines; g++ {
		g := g
		go func() {
			defer wg.Done()
			addr := &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 10000 + g}
			payload := make([]byte, 8)
			for i := 0; i < perGoroutine; i++ {
				if _, err := c.WriteTo(payload, addr); err != nil {
					t.Errorf("WriteTo: %v", err)
				}
			}
		}()
	}
	wg.Wait()
	if err := c.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// No corruption/panic and Close() drained cleanly; exact count may be
	// lower than goroutines*perGoroutine if the queue filled under load —
	// that's the documented drop behavior, not a bug.
	if got := fake.writeCount(); got == 0 {
		t.Fatal("no writes reached the underlying conn at all")
	}
}
