package netconn

import (
	"net"
)

// NonBlockingPacketConn wraps a net.PacketConn and drops packets on WriteTo
// if the underlying WriteTo would block (simulated via a buffered channel).
// This is critical for tunneling KCP over TCP, as KCP's update loop will freeze
// if WriteTo blocks, leading to spurious timeouts.
type NonBlockingPacketConn struct {
	net.PacketConn
	ch   chan []byte
	peer net.Addr
}

func NewNonBlockingPacketConn(pc net.PacketConn, queueSize int) *NonBlockingPacketConn {
	npc := &NonBlockingPacketConn{
		PacketConn: pc,
		ch:         make(chan []byte, queueSize),
	}
	go npc.loop()
	return npc
}

func (c *NonBlockingPacketConn) loop() {
	for b := range c.ch {
		_, _ = c.PacketConn.WriteTo(b, c.peer) // Ignore errors, connection will be closed elsewhere if fatal
	}
}

func (c *NonBlockingPacketConn) WriteTo(p []byte, addr net.Addr) (int, error) {
	c.peer = addr // Assuming peer is static for this connection (like in TURN)
	buf := make([]byte, len(p))
	copy(buf, p)
	select {
	case c.ch <- buf:
		return len(p), nil
	default:
		// Drop packet, return success so caller doesn't error out
		return len(p), nil
	}
}

func (c *NonBlockingPacketConn) Close() error {
	err := c.PacketConn.Close()
	// Channel intentionally not closed to avoid panics on concurrent writes
	return err
}
