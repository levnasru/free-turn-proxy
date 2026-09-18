package stunprobe

import (
	"context"
	"encoding/binary"
	"net"
	"sync"
	"testing"
	"time"
)

// startMockSTUNServer starts an in-process UDP server that answers STUN Binding Requests.
// delay controls how long before answering (to simulate distance/RTT).
func startMockSTUNServer(t *testing.T, delay time.Duration) (string, func()) {
	t.Helper()
	conn, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen mock stun: %v", err)
	}

	done := make(chan struct{})
	go func() {
		buf := make([]byte, 512)
		for {
			n, addr, err := conn.ReadFrom(buf)
			if err != nil {
				return
			}
			if n >= stunHeaderLen && binary.BigEndian.Uint16(buf[0:2]) == stunBindingReq {
				resp := make([]byte, stunHeaderLen)
				binary.BigEndian.PutUint16(resp[0:2], stunBindingResp)
				binary.BigEndian.PutUint16(resp[2:4], 0)
				binary.BigEndian.PutUint32(resp[4:8], stunMagicCookie)
				copy(resp[8:20], buf[8:20]) // echo transaction ID

				go func(b []byte, a net.Addr) {
					if delay > 0 {
						time.Sleep(delay)
					}
					_, _ = conn.WriteTo(b, a)
				}(resp, addr)
			}
		}
	}()

	cleanup := func() {
		_ = conn.Close()
		close(done)
	}
	return conn.LocalAddr().String(), cleanup
}

func TestProbe_Success(t *testing.T) {
	t.Parallel()
	addr, cleanup := startMockSTUNServer(t, 5*time.Millisecond)
	defer cleanup()

	rtt, err := Probe(addr, 500*time.Millisecond)
	if err != nil {
		t.Fatalf("Probe failed: %v", err)
	}
	if rtt <= 0 {
		t.Fatalf("expected positive RTT, got %v", rtt)
	}
}

func TestProbe_Timeout(t *testing.T) {
	t.Parallel()
	// Listen without replying
	conn, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer conn.Close()

	_, err = Probe(conn.LocalAddr().String(), 50*time.Millisecond)
	if err == nil {
		t.Fatal("expected timeout error")
	}
}

func TestProbe_MalformedResponse(t *testing.T) {
	t.Parallel()
	conn, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer conn.Close()

	go func() {
		buf := make([]byte, 512)
		n, addr, err := conn.ReadFrom(buf)
		if err == nil && n > 0 {
			// Reply with wrong magic cookie
			resp := make([]byte, stunHeaderLen)
			binary.BigEndian.PutUint16(resp[0:2], stunBindingResp)
			binary.BigEndian.PutUint32(resp[4:8], 0xDEADBEEF)
			_, _ = conn.WriteTo(resp, addr)
		}
	}()

	_, err = Probe(conn.LocalAddr().String(), 500*time.Millisecond)
	if err == nil {
		t.Fatal("expected error on malformed STUN response")
	}
}

func TestRanker_FilterHomogeneous(t *testing.T) {
	t.Parallel()

	// 5 Fast servers (0ms extra delay)
	var fastAddrs []string
	for i := 0; i < 5; i++ {
		addr, cleanup := startMockSTUNServer(t, 0)
		defer cleanup()
		fastAddrs = append(fastAddrs, addr)
	}

	// 2 Slow servers (80ms extra delay)
	var slowAddrs []string
	for i := 0; i < 2; i++ {
		addr, cleanup := startMockSTUNServer(t, 80*time.Millisecond)
		defer cleanup()
		slowAddrs = append(slowAddrs, addr)
	}

	all := append([]string(nil), slowAddrs...)
	all = append(all, fastAddrs...)

	ranker := NewRanker(Config{
		Timeout:           200 * time.Millisecond,
		MinClusterSize:    5,
		ToleranceAbsolute: 25 * time.Millisecond,
		ToleranceRatio:    1.8,
	})

	ctx := context.Background()
	result := ranker.FilterHomogeneous(ctx, all)

	if len(result) != 5 {
		t.Fatalf("expected 5 fast candidates, got %d: %v", len(result), result)
	}

	// Ensure none of the slow servers are in the result
	for _, slow := range slowAddrs {
		for _, res := range result {
			if res == slow {
				t.Fatalf("slow server %s unexpectedly found in fast cluster result", slow)
			}
		}
	}
}

func TestRanker_FilterHomogeneous_SmallFastCluster(t *testing.T) {
	t.Parallel()

	// 3 Fast servers
	var fastAddrs []string
	for i := 0; i < 3; i++ {
		addr, cleanup := startMockSTUNServer(t, 0)
		defer cleanup()
		fastAddrs = append(fastAddrs, addr)
	}

	// 2 Slow servers
	var slowAddrs []string
	for i := 0; i < 2; i++ {
		addr, cleanup := startMockSTUNServer(t, 80*time.Millisecond)
		defer cleanup()
		slowAddrs = append(slowAddrs, addr)
	}

	all := append([]string(nil), slowAddrs...)
	all = append(all, fastAddrs...)

	// MinClusterSize is 10, but we have 3 fast servers. Ranker should still pick the 3 fast ones!
	ranker := NewRanker(Config{
		Timeout:           200 * time.Millisecond,
		MinClusterSize:    10,
		ToleranceAbsolute: 25 * time.Millisecond,
		ToleranceRatio:    1.8,
	})

	ctx := context.Background()
	result := ranker.FilterHomogeneous(ctx, all)

	if len(result) != 3 {
		t.Fatalf("expected 3 fast candidates, got %d: %v", len(result), result)
	}
	for _, slow := range slowAddrs {
		for _, res := range result {
			if res == slow {
				t.Fatalf("slow server %s unexpectedly found in fast cluster result", slow)
			}
		}
	}
}

func TestRanker_FallbackWhenNoneRespond(t *testing.T) {
	t.Parallel()
	unreachable := []string{"127.0.0.1:59991", "127.0.0.1:59992"}

	ranker := NewRanker(Config{
		Timeout: 50 * time.Millisecond,
	})

	ctx := context.Background()
	result := ranker.FilterHomogeneous(ctx, unreachable)

	if len(result) != len(unreachable) {
		t.Fatalf("expected fallback to original %d candidates, got %d", len(unreachable), len(result))
	}
}

func TestRanker_CacheAndInvalidate(t *testing.T) {
	t.Parallel()
	var probeCount int
	var mu sync.Mutex

	conn, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer conn.Close()

	go func() {
		buf := make([]byte, 512)
		for {
			_, addr, err := conn.ReadFrom(buf)
			if err != nil {
				return
			}
			mu.Lock()
			probeCount++
			mu.Unlock()
			resp := make([]byte, stunHeaderLen)
			binary.BigEndian.PutUint16(resp[0:2], stunBindingResp)
			binary.BigEndian.PutUint32(resp[4:8], stunMagicCookie)
			copy(resp[8:20], buf[8:20])
			_, _ = conn.WriteTo(resp, addr)
		}
	}()

	target := conn.LocalAddr().String()
	ranker := NewRanker(Config{
		Timeout:        300 * time.Millisecond,
		MinClusterSize: 1,
	})

	ctx := context.Background()
	_ = ranker.FilterHomogeneous(ctx, []string{target, "127.0.0.1:59999"})

	mu.Lock()
	firstCount := probeCount
	mu.Unlock()

	if firstCount == 0 {
		t.Fatal("expected at least 1 probe on first call")
	}

	// Second call should hit cache, probeCount should not increase
	_ = ranker.FilterHomogeneous(ctx, []string{target, "127.0.0.1:59999"})
	mu.Lock()
	secondCount := probeCount
	mu.Unlock()

	if secondCount != firstCount {
		t.Fatalf("expected probeCount to remain %d, got %d (cache miss)", firstCount, secondCount)
	}

	// Invalidate cache and call again: probeCount must increase
	ranker.Invalidate()
	_ = ranker.FilterHomogeneous(ctx, []string{target, "127.0.0.1:59999"})
	mu.Lock()
	thirdCount := probeCount
	mu.Unlock()

	if thirdCount <= secondCount {
		t.Fatalf("expected probeCount to increase after Invalidate, got %d", thirdCount)
	}
}
