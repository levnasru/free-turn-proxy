package udpserver

import (
	"context"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/samosvalishe/free-turn-proxy/internal/logx"
	"github.com/samosvalishe/free-turn-proxy/internal/wire/reseq"
)

// mockPacketConn emulates a DTLS datagram connection in memory.
type mockPacketConn struct {
	readCh  chan []byte
	writeCh chan []byte
	closed  chan struct{}
	once    sync.Once
}

func newMockPacketConn() (*mockPacketConn, *mockPacketConn) {
	c1 := make(chan []byte, 32)
	c2 := make(chan []byte, 32)
	done1 := make(chan struct{})
	done2 := make(chan struct{})

	conn1 := &mockPacketConn{readCh: c1, writeCh: c2, closed: done1}
	conn2 := &mockPacketConn{readCh: c2, writeCh: c1, closed: done2}
	return conn1, conn2
}

func (m *mockPacketConn) Read(b []byte) (int, error) {
	select {
	case <-m.closed:
		return 0, net.ErrClosed
	case data, ok := <-m.readCh:
		if !ok {
			return 0, net.ErrClosed
		}
		n := copy(b, data)
		return n, nil
	}
}

func (m *mockPacketConn) Write(b []byte) (int, error) {
	select {
	case <-m.closed:
		return 0, net.ErrClosed
	default:
	}
	pkt := make([]byte, len(b))
	copy(pkt, b)
	select {
	case <-m.closed:
		return 0, net.ErrClosed
	case m.writeCh <- pkt:
		return len(b), nil
	}
}

func (m *mockPacketConn) Close() error {
	m.once.Do(func() {
		close(m.closed)
	})
	return nil
}

func (m *mockPacketConn) LocalAddr() net.Addr                { return &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 1234} }
func (m *mockPacketConn) RemoteAddr() net.Addr               { return &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 5678} }
func (m *mockPacketConn) SetDeadline(t time.Time) error      { return nil }
func (m *mockPacketConn) SetReadDeadline(t time.Time) error  { return nil }
func (m *mockPacketConn) SetWriteDeadline(t time.Time) error { return nil }

func TestUDPServerRegistryMultiStreamBatching(t *testing.T) {
	// 1. Start a local UDP backend (mocking WireGuard)
	backend, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen backend: %v", err)
	}
	defer backend.Close()
	backendAddr := backend.LocalAddr().String()

	logger := logx.New(false)
	reg := NewRegistry(Deps{Log: logger, BatchSize: 4})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// 2. Connect 3 simulated streams from the same client sequentially
	const numStreams = 3
	type streamPair struct {
		clientSide *mockPacketConn
		serverSide *mockPacketConn
	}
	streams := make([]streamPair, numStreams)
	for i := 0; i < numStreams; i++ {
		cSide, sSide := newMockPacketConn()
		streams[i] = streamPair{clientSide: cSide, serverSide: sSide}

		go reg.Handle(ctx, logger, sSide, backendAddr, "client-test-123")
		time.Sleep(20 * time.Millisecond) // ensure deterministic slot ordering
	}

	// 3. Send 1 packet from each stream to backend; verify backend receives them from the SAME source address!
	var commonSender net.Addr
	for i := 0; i < numStreams; i++ {
		msg := []byte("hello-from-stream")
		if _, werr := streams[i].clientSide.Write(msg); werr != nil {
			t.Fatalf("stream %d write: %v", i, werr)
		}

		buf := make([]byte, 1024)
		_ = backend.SetReadDeadline(time.Now().Add(2 * time.Second))
		n, sender, rerr := backend.ReadFrom(buf)
		if rerr != nil {
			t.Fatalf("backend read packet %d: %v", i, rerr)
		}
		if string(buf[:n]) != string(msg) {
			t.Fatalf("backend got %q, want %q", string(buf[:n]), string(msg))
		}

		if commonSender == nil {
			commonSender = sender
		} else if commonSender.String() != sender.String() {
			t.Fatalf("endpoint mismatch! stream %d arrived from %s, expected %s (endpoint roaming inversion not neutralized)",
				i, sender.String(), commonSender.String())
		}
	}

	// 4. Send 12 packets from backend to commonSender; verify micro-batching:
	for i := 0; i < 12; i++ {
		pkt := []byte{byte(i)}
		if _, werr := backend.WriteTo(pkt, commonSender); werr != nil {
			t.Fatalf("backend send packet %d: %v", i, werr)
		}
	}

	// Collect packets received by each stream
	batches := make([][]byte, numStreams)
	for sIdx := 0; sIdx < numStreams; sIdx++ {
		for pIdx := 0; pIdx < 4; pIdx++ {
			select {
			case pkt := <-streams[sIdx].clientSide.readCh:
				batches[sIdx] = append(batches[sIdx], pkt...)
			case <-time.After(2 * time.Second):
				t.Fatalf("stream %d timed out waiting for packet %d (got %d packets so far)", sIdx, pIdx, len(batches[sIdx]))
			}
		}
	}

	// Verify each stream received exactly 4 packets
	for sIdx, b := range batches {
		if len(b) != 4 {
			t.Fatalf("stream %d expected 4 packets, got %d", sIdx, len(b))
		}
		// Verify consecutive sequence within each batch
		for k := 1; k < len(b); k++ {
			if b[k] != b[k-1]+1 {
				t.Fatalf("stream %d packets out of order within micro-batch: %v", sIdx, b)
			}
		}
	}
}

func TestUDPServerRegistryMultiClientIsolation(t *testing.T) {
	backend, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen backend: %v", err)
	}
	defer backend.Close()
	backendAddr := backend.LocalAddr().String()

	logger := logx.New(false)
	reg := NewRegistry(Deps{Log: logger, BatchSize: 4})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Client A
	cAClient, cAServer := newMockPacketConn()
	go reg.Handle(ctx, logger, cAServer, backendAddr, "client-A")

	// Client B
	cBClient, cBServer := newMockPacketConn()
	go reg.Handle(ctx, logger, cBServer, backendAddr, "client-B")

	time.Sleep(50 * time.Millisecond)

	// Send from Client A
	_, _ = cAClient.Write([]byte("msgA"))
	buf := make([]byte, 1024)
	_ = backend.SetReadDeadline(time.Now().Add(2 * time.Second))
	_, addrA, err := backend.ReadFrom(buf)
	if err != nil {
		t.Fatalf("read A: %v", err)
	}

	// Send from Client B
	_, _ = cBClient.Write([]byte("msgB"))
	_, addrB, err := backend.ReadFrom(buf)
	if err != nil {
		t.Fatalf("read B: %v", err)
	}

	if addrA.String() == addrB.String() {
		t.Fatalf("clients A and B must have different backend sockets, both got %s", addrA.String())
	}

	// Send reply to A only
	_, _ = backend.WriteTo([]byte("replyA"), addrA)

	select {
	case pkt := <-cAClient.readCh:
		if string(pkt) != "replyA" {
			t.Fatalf("client A got %s, want replyA", string(pkt))
		}
	case <-time.After(1 * time.Second):
		t.Fatalf("client A timed out waiting for reply")
	}

	// Ensure B got nothing
	select {
	case pkt := <-cBClient.readCh:
		t.Fatalf("client B unexpectedly received %s", string(pkt))
	case <-time.After(100 * time.Millisecond):
		// OK
	}
}

func TestUDPServerRegistryStreamFailover(t *testing.T) {
	backend, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen backend: %v", err)
	}
	defer backend.Close()
	backendAddr := backend.LocalAddr().String()

	logger := logx.New(false)
	reg := NewRegistry(Deps{Log: logger, BatchSize: 4})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	const numStreams = 3
	type streamPair struct {
		clientSide *mockPacketConn
		serverSide *mockPacketConn
	}
	streams := make([]streamPair, numStreams)
	for i := 0; i < numStreams; i++ {
		cSide, sSide := newMockPacketConn()
		streams[i] = streamPair{clientSide: cSide, serverSide: sSide}

		go reg.Handle(ctx, logger, sSide, backendAddr, "client-failover")
		time.Sleep(20 * time.Millisecond)
	}

	// Ping backend to acquire sender address
	_, _ = streams[0].clientSide.Write([]byte("ping"))
	buf := make([]byte, 1024)
	_ = backend.SetReadDeadline(time.Now().Add(2 * time.Second))
	_, commonSender, rerr := backend.ReadFrom(buf)
	if rerr != nil {
		t.Fatalf("read from backend: %v", rerr)
	}

	// Disconnect stream 1
	_ = streams[1].clientSide.Close()
	_ = streams[1].serverSide.Close()
	time.Sleep(50 * time.Millisecond)

	// Send 8 packets (2 batches of 4) -> should be distributed across streams 0 and 2
	for i := 0; i < 8; i++ {
		_, _ = backend.WriteTo([]byte{byte(10 + i)}, commonSender)
	}

	// Streams 0 and 2 should each receive 4 packets
	for _, idx := range []int{0, 2} {
		for k := 0; k < 4; k++ {
			select {
			case <-streams[idx].clientSide.readCh:
				// OK
			case <-time.After(2 * time.Second):
				t.Fatalf("stream %d timed out waiting for failover packet %d", idx, k)
			}
		}
	}
}

func TestUDPServerStandalone(t *testing.T) {
	backend, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen backend: %v", err)
	}
	defer backend.Close()
	backendAddr := backend.LocalAddr().String()

	logger := logx.New(false)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	cSide, sSide := newMockPacketConn()
	go Handle(ctx, logger, sSide, backendAddr)

	time.Sleep(30 * time.Millisecond)

	// Client sends to backend
	_, _ = cSide.Write([]byte("ping-standalone"))
	buf := make([]byte, 1024)
	_ = backend.SetReadDeadline(time.Now().Add(2 * time.Second))
	n, sender, err := backend.ReadFrom(buf)
	if err != nil {
		t.Fatalf("read backend: %v", err)
	}
	if string(buf[:n]) != "ping-standalone" {
		t.Fatalf("got %s, want ping-standalone", string(buf[:n]))
	}

	// Backend replies
	_, _ = backend.WriteTo([]byte("pong-standalone"), sender)
	select {
	case pkt := <-cSide.readCh:
		if string(pkt) != "pong-standalone" {
			t.Fatalf("got %s, want pong-standalone", string(pkt))
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("timeout waiting for standalone reply")
	}
}

func TestUDPServerResequencingMultiStream(t *testing.T) {
	backend, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen backend: %v", err)
	}
	defer func() { _ = backend.Close() }()
	backendAddr := backend.LocalAddr().String()

	logger := logx.Nop()
	reg := NewRegistry(Deps{Log: logger, BatchSize: 2})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	clientID := "client-reseq-test"
	c1, s1 := newMockPacketConn()
	c2, s2 := newMockPacketConn()

	go reg.Handle(ctx, logger, s1, backendAddr, clientID)
	go reg.Handle(ctx, logger, s2, backendAddr, clientID)

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		reg.mu.Lock()
		sess := reg.sessions[clientID+"@"+backendAddr]
		reg.mu.Unlock()
		if sess != nil {
			sess.slotsMu.Lock()
			count := len(sess.slots)
			sess.slotsMu.Unlock()
			if count == 2 {
				break
			}
		}
		time.Sleep(5 * time.Millisecond)
	}

	t.Logf("sending p1")
	p1 := reseq.Wrap(nil, []byte("data-1"), 1, 0)
	_, _ = c1.Write(p1)

	t.Logf("sending p3")
	p3 := reseq.Wrap(nil, []byte("data-3"), 3, 0)
	_, _ = c2.Write(p3)

	t.Logf("sending p2")
	p2 := reseq.Wrap(nil, []byte("data-2"), 2, 0)
	_, _ = c1.Write(p2)

	buf := make([]byte, 1024)
	var received []string
	for i := 0; i < 3; i++ {
		_ = backend.SetReadDeadline(time.Now().Add(2 * time.Second))
		n, _, rerr := backend.ReadFrom(buf)
		if rerr != nil {
			t.Fatalf("failed reading packet %d from backend: %v", i+1, rerr)
		}
		t.Logf("received packet %d: %s", i+1, string(buf[:n]))
		received = append(received, string(buf[:n]))
	}

	if received[0] != "data-1" || received[1] != "data-2" || received[2] != "data-3" {
		t.Fatalf("expected packets in order [data-1, data-2, data-3], got %v", received)
	}

	// Now test downlink: backend sends reply
	// Get sender from backend
	_ = backend.SetReadDeadline(time.Now().Add(100 * time.Millisecond))
	// Send another packet from client so backend has sender
	p4 := reseq.Wrap(nil, []byte("ping"), 4, 0)
	_, _ = c1.Write(p4)
	n, sender, err := backend.ReadFrom(buf)
	if err != nil {
		t.Fatalf("read ping: %v", err)
	}
	if string(buf[:n]) != "ping" {
		t.Fatalf("got %s, want ping", string(buf[:n]))
	}

	// Backend replies
	_, _ = backend.WriteTo([]byte("downlink-data"), sender)

	// One of the client streams must receive the packet wrapped with reseq
	var gotDownlink []byte
	select {
	case pkt := <-c1.readCh:
		gotDownlink = pkt
	case pkt := <-c2.readCh:
		gotDownlink = pkt
	case <-time.After(2 * time.Second):
		t.Fatalf("timeout waiting for downlink packet")
	}

	if !reseq.IsReseq(gotDownlink) {
		t.Fatalf("downlink packet should have reseq magic 0xD5, got %v", gotDownlink)
	}
	seq, _, payload, ok := reseq.Unwrap(gotDownlink)
	if !ok || string(payload) != "downlink-data" || seq == 0 {
		t.Fatalf("invalid reseq unwrap: ok=%v, seq=%d, payload=%s", ok, seq, string(payload))
	}
}

func TestUDPServerEpochGhostSlotElimination(t *testing.T) {
	logger := logx.New(false)
	reg := NewRegistry(Deps{Log: logger, BatchSize: 1})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	backend, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("backend listen: %v", err)
	}
	defer backend.Close()
	backendAddr := backend.LocalAddr().String()

	clientID := "client-epoch-test"
	s1, c1 := newMockPacketConn()
	s2, c2 := newMockPacketConn()

	go reg.Handle(ctx, logger, s1, backendAddr, clientID)

	// Wait for slot 1
	time.Sleep(20 * time.Millisecond)

	// Send packet with epoch 100 on slot 1
	p1 := reseq.Wrap(nil, []byte("epoch-100-data"), 1, 100)
	_, _ = c1.Write(p1)

	buf := make([]byte, 1024)
	_ = backend.SetReadDeadline(time.Now().Add(2 * time.Second))
	n, _, rerr := backend.ReadFrom(buf)
	if rerr != nil {
		t.Fatalf("read backend epoch 100: %v", rerr)
	}
	if string(buf[:n]) != "epoch-100-data" {
		t.Fatalf("got %s, want epoch-100-data", string(buf[:n]))
	}

	// Now client restarts! Slot 2 connects with epoch 200
	go reg.Handle(ctx, logger, s2, backendAddr, clientID)
	time.Sleep(20 * time.Millisecond)

	// Send packet with epoch 200 on slot 2
	p2 := reseq.Wrap(nil, []byte("epoch-200-handshake"), 1, 200)
	_, _ = c2.Write(p2)

	_ = backend.SetReadDeadline(time.Now().Add(2 * time.Second))
	n, sender2, rerr := backend.ReadFrom(buf)
	if rerr != nil {
		t.Fatalf("read backend epoch 200: %v", rerr)
	}
	if string(buf[:n]) != "epoch-200-handshake" {
		t.Fatalf("got %s, want epoch-200-handshake", string(buf[:n]))
	}

	// Verify that session pruned slot 1!
	reg.mu.Lock()
	sess := reg.sessions[clientID+"@"+backendAddr]
	reg.mu.Unlock()
	if sess == nil {
		t.Fatalf("session is nil")
	}

	sess.slotsMu.Lock()
	slotCount := len(sess.slots)
	sessEpoch := sess.currentEpoch
	sess.slotsMu.Unlock()

	if sessEpoch != 200 {
		t.Errorf("expected session epoch 200, got %d", sessEpoch)
	}
	if slotCount != 1 {
		t.Errorf("expected exactly 1 slot after pruning ghost slot, got %d", slotCount)
	}

	// Backend replies to sender2: downlink MUST go to c2, NOT c1
	_, _ = backend.WriteTo([]byte("downlink-reply"), sender2)

	select {
	case pkt := <-c2.readCh:
		seq, ep, payload, ok := reseq.Unwrap(pkt)
		if !ok || ep != 200 || seq != 1 || string(payload) != "downlink-reply" {
			t.Fatalf("unexpected downlink on c2: ok=%v ep=%d seq=%d payload=%s", ok, ep, seq, string(payload))
		}
	case pkt := <-c1.readCh:
		t.Fatalf("downlink was routed to dead ghost slot c1! pkt=%v", pkt)
	case <-time.After(2 * time.Second):
		t.Fatalf("timeout waiting for downlink on c2")
	}
}

func TestUDPServerZeroEpochGhostSlotElimination(t *testing.T) {
	logger := logx.New(false)
	reg := NewRegistry(Deps{Log: logger, BatchSize: 1})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	backend, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("backend listen: %v", err)
	}
	defer backend.Close()
	backendAddr := backend.LocalAddr().String()

	clientID := "client-zero-epoch-test"
	s1, c1 := newMockPacketConn()
	s2, c2 := newMockPacketConn()
	s3, _ := newMockPacketConn()
	s4, c4 := newMockPacketConn()

	// Session 1: slot 1 and slot 2 connect
	go reg.Handle(ctx, logger, s1, backendAddr, clientID)
	go reg.Handle(ctx, logger, s2, backendAddr, clientID)
	time.Sleep(20 * time.Millisecond)

	// Slot 1 sends packet with epoch 100
	p1 := reseq.Wrap(nil, []byte("epoch-100-packet"), 1, 100)
	_, _ = c1.Write(p1)

	buf := make([]byte, 1024)
	_ = backend.SetReadDeadline(time.Now().Add(2 * time.Second))
	n, _, rerr := backend.ReadFrom(buf)
	if rerr != nil {
		t.Fatalf("read backend epoch 100: %v", rerr)
	}
	if string(buf[:n]) != "epoch-100-packet" {
		t.Fatalf("got %s, want epoch-100-packet", string(buf[:n]))
	}

	// Slot 2 never sent any packets (epoch remains 0).
	// Simulate time passing (slot 2 was created in previous session)
	reg.mu.Lock()
	sess := reg.sessions[clientID+"@"+backendAddr]
	reg.mu.Unlock()
	sess.slotsMu.Lock()
	for _, sl := range sess.slots {
		if sl.conn == s2 {
			sl.createdAt = time.Now().Add(-10 * time.Second)
		}
	}
	sess.slotsMu.Unlock()

	// Session 2: slot 3 and slot 4 connect
	go reg.Handle(ctx, logger, s3, backendAddr, clientID)
	go reg.Handle(ctx, logger, s4, backendAddr, clientID)
	time.Sleep(20 * time.Millisecond)

	// Slot 4 sends first packet with epoch 200 (e.g. WireGuard handshake)
	p4 := reseq.Wrap(nil, []byte("epoch-200-handshake"), 1, 200)
	_, _ = c4.Write(p4)

	_ = backend.SetReadDeadline(time.Now().Add(2 * time.Second))
	n, sender4, rerr := backend.ReadFrom(buf)
	if rerr != nil {
		t.Fatalf("read backend epoch 200: %v", rerr)
	}
	if string(buf[:n]) != "epoch-200-handshake" {
		t.Fatalf("got %s, want epoch-200-handshake", string(buf[:n]))
	}

	// Verify pruning:
	// Slot 1 (epoch 100) must be pruned.
	// Slot 2 (epoch 0, age > 5s) MUST be pruned!
	// Slot 3 (epoch 0, age < 5s) kept.
	// Slot 4 (epoch 200) kept.
	sess.slotsMu.Lock()
	count := len(sess.slots)
	sess.slotsMu.Unlock()
	if count != 2 {
		t.Fatalf("expected exactly 2 slots (slot 3 and slot 4), got %d", count)
	}

	// Backend replies to sender4: downlink MUST route to slot 4 (curEpoch verified),
	// and NEVER to slot 2 (pruned) or unverified slot 3!
	_, _ = backend.WriteTo([]byte("downlink-handshake-response"), sender4)

	select {
	case pkt := <-c4.readCh:
		seq, ep, payload, ok := reseq.Unwrap(pkt)
		if !ok || ep != 200 || seq != 1 || string(payload) != "downlink-handshake-response" {
			t.Fatalf("unexpected downlink on c4: ok=%v ep=%d seq=%d payload=%s", ok, ep, seq, string(payload))
		}
	case pkt := <-c2.readCh:
		t.Fatalf("downlink was routed to dead ghost slot c2! pkt=%v", pkt)
	case <-time.After(2 * time.Second):
		t.Fatalf("timeout waiting for downlink on c4")
	}
}

