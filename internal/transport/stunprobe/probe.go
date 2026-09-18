package stunprobe

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"sync"
	"time"

	"github.com/samosvalishe/free-turn-proxy/internal/netctl"
)

const (
	stunMagicCookie uint32 = 0x2112A442
	stunBindingReq  uint16 = 0x0001
	stunBindingResp uint16 = 0x0101
	stunHeaderLen          = 20
)

// Probe sends a standard RFC 5389 STUN Binding Request over UDP to target (host:port)
// without authentication, measuring the round-trip latency to the candidate relay.
func Probe(target string, timeout time.Duration) (time.Duration, error) {
	d := &net.Dialer{
		Timeout: timeout,
		Control: netctl.Apply,
	}
	conn, err := d.Dial("udp", target)
	if err != nil {
		return 0, err
	}
	defer conn.Close()

	req := make([]byte, stunHeaderLen)
	binary.BigEndian.PutUint16(req[0:2], stunBindingReq)
	binary.BigEndian.PutUint16(req[2:4], 0) // zero attributes
	binary.BigEndian.PutUint32(req[4:8], stunMagicCookie)
	if _, err := rand.Read(req[8:20]); err != nil {
		return 0, fmt.Errorf("generate transaction id: %w", err)
	}

	t0 := time.Now()
	if err := conn.SetDeadline(t0.Add(timeout)); err != nil {
		return 0, err
	}

	if _, err := conn.Write(req); err != nil {
		return 0, err
	}

	resp := make([]byte, 512)
	n, err := conn.Read(resp)
	if err != nil {
		return 0, err
	}
	rtt := time.Since(t0)

	if n < stunHeaderLen {
		return 0, fmt.Errorf("short response: %d bytes", n)
	}
	msgType := binary.BigEndian.Uint16(resp[0:2])
	if msgType != stunBindingResp {
		return 0, fmt.Errorf("unexpected STUN message type: 0x%04x", msgType)
	}
	cookie := binary.BigEndian.Uint32(resp[4:8])
	if cookie != stunMagicCookie {
		return 0, fmt.Errorf("invalid STUN magic cookie: 0x%08x", cookie)
	}
	if !bytes.Equal(resp[8:20], req[8:20]) {
		return 0, errors.New("STUN transaction ID mismatch")
	}

	return rtt, nil
}

// ProbeAll probes multiple targets concurrently with a worker pool semaphore.
// Returns a map of target -> measured RTT for all responsive targets.
func ProbeAll(ctx context.Context, targets []string, timeout time.Duration, concurrency int) map[string]time.Duration {
	if concurrency <= 0 {
		concurrency = 20
	}
	results := make(map[string]time.Duration, len(targets))
	var mu sync.Mutex

	sem := make(chan struct{}, concurrency)
	var wg sync.WaitGroup

	for _, target := range targets {
		if ctx.Err() != nil {
			break
		}
		wg.Add(1)
		go func(t string) {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
			case <-ctx.Done():
				return
			}

			rtt, err := Probe(t, timeout)
			if err == nil {
				mu.Lock()
				results[t] = rtt
				mu.Unlock()
			}
		}(target)
	}

	wg.Wait()
	return results
}
