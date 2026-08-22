package netconn

import (
	"net"
	"sync"
)

// NonBlockingPacketConn wraps a net.PacketConn so WriteTo never blocks the
// caller — critical for tunneling KCP over TCP, since KCP's update loop
// runs synchronously with WriteTo and a blocking write there stalls the
// whole loop, surfacing as a spurious timeout rather than a clean error.
// Writes queue onto a buffered channel; a background goroutine drains it
// into the real connection. A full queue drops the packet silently (the
// caller still sees success) — correct for KCP data and most TURN control
// traffic, which both have their own retransmit/loss-recovery above this
// layer; see turndial.Open's close sequence for the one path (the
// delete-allocation Refresh) that depends on Close draining the queue
// before the real conn closes, rather than on this drop behavior.
type NonBlockingPacketConn struct {
	net.PacketConn
	ch        chan outboundPacket
	done      chan struct{}
	stopped   chan struct{}
	closeOnce sync.Once
}

type outboundPacket struct {
	b    []byte
	addr net.Addr
}

func NewNonBlockingPacketConn(pc net.PacketConn, queueSize int) *NonBlockingPacketConn {
	npc := &NonBlockingPacketConn{
		PacketConn: pc,
		ch:         make(chan outboundPacket, queueSize),
		done:       make(chan struct{}),
		stopped:    make(chan struct{}),
	}
	go npc.loop()
	return npc
}

func (c *NonBlockingPacketConn) loop() {
	defer close(c.stopped)
	for {
		select {
		case p := <-c.ch:
			_, _ = c.PacketConn.WriteTo(p.b, p.addr)
		case <-c.done:
			// Drain whatever's left so a packet queued right before Close
			// (e.g. TURN's delete-allocation Refresh) still reaches the
			// wire before the real conn closes underneath us.
			for {
				select {
				case p := <-c.ch:
					_, _ = c.PacketConn.WriteTo(p.b, p.addr)
				default:
					return
				}
			}
		}
	}
}

func (c *NonBlockingPacketConn) WriteTo(p []byte, addr net.Addr) (int, error) {
	select {
	case <-c.done:
		return 0, net.ErrClosed
	default:
	}
	buf := make([]byte, len(p))
	copy(buf, p)
	select {
	case c.ch <- outboundPacket{buf, addr}:
		return len(p), nil
	default:
		// Drop: caller reports success, loss recovery happens above this
		// layer (KCP ARQ, TURN transaction retransmit). See type doc.
		return len(p), nil
	}
}

func (c *NonBlockingPacketConn) Close() error {
	c.closeOnce.Do(func() { close(c.done) })
	<-c.stopped // wait for the drain to finish before the real conn closes
	return c.PacketConn.Close()
}
