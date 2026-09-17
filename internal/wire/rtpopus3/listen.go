package rtpopus3

import (
	"errors"
	"fmt"
	"net"
	"sync"
	"time"

	dtlsnet "github.com/pion/dtls/v3/pkg/net"
	pionudp "github.com/pion/transport/v4/udp"
)

var bufPool = sync.Pool{
	New: func() any {
		b := make([]byte, 1600+overhead+markerLen)
		return &b
	},
}

func Listen(addr *net.UDPAddr, key []byte) (dtlsnet.PacketListener, error) {
	state, err := NewState(key)
	if err != nil {
		return nil, err
	}
	inner, err := pionudp.Listen("udp", addr)
	if err != nil {
		return nil, fmt.Errorf("rtpopus3:udp listen: %w", err)
	}
	return &packetListener{
		inner: dtlsnet.PacketListenerFromListener(inner),
		state: state,
	}, nil
}

type packetListener struct {
	inner dtlsnet.PacketListener
	state *State
}

func (l *packetListener) Accept() (net.PacketConn, net.Addr, error) {
	pc, addr, err := l.inner.Accept()
	if err != nil {
		return pc, addr, err
	}
	conn, err := NewConnFromState(l.state, true)
	if err != nil {
		return nil, addr, err
	}
	// ponytail: heisenbug mitigation, not a root-cause fix - exact race is
	// somewhere between pion/dtls's first read on a freshly Accept()'d conn
	// and this conn's registration in pionudp.Listen (traced through
	// pionudp.Listen + pion/dtls conn.go/listener.go/netctx, no smoking gun
	// found; disappears under any added scheduling delay - println, -race
	// build - which is why a delay fixes it without knowing why). Reproduces
	// 100% on very-low-jitter loopback-through-relay paths (self-hosted
	// client+server both dialing out through the same TURN relay); never
	// observed on real family traffic, which has enough natural network
	// jitter to miss the window on its own. Accept() runs once per
	// connection, not per packet, so this costs nothing on the hot relay
	// path. Upgrade path: file upstream against pion/dtls with a repro, or
	// bisect pion/dtls's conn.go readAndBuffer/handshake goroutines directly.
	time.Sleep(3 * time.Millisecond)
	return &packetConn{inner: pc, conn: conn}, addr, nil
}

func (l *packetListener) Close() error   { return l.inner.Close() }
func (l *packetListener) Addr() net.Addr { return l.inner.Addr() }

type packetConn struct {
	inner net.PacketConn
	conn  *Conn
}

func (c *packetConn) ReadFrom(p []byte) (int, net.Addr, error) {
	bp := bufPool.Get().(*[]byte) //nolint:errcheck // pool New always returns *[]byte
	buf := *bp
	need := len(p) + overhead
	if cap(buf) < need {
		buf = make([]byte, need)
		*bp = buf
	}
	defer bufPool.Put(bp)

	n, addr, err := c.inner.ReadFrom(buf[:cap(buf)])
	if err != nil {
		return 0, addr, err
	}
	wire := buf[:n]
	if len(wire) < legacyOverhead {
		return 0, addr, errors.New("rtpopus3:packet too short")
	}
	m, err := c.conn.Unwrap(wire, p)
	if err != nil {
		return 0, addr, err
	}
	return m, addr, nil
}

func (c *packetConn) WriteTo(p []byte, addr net.Addr) (int, error) {
	// MaxWire, не overhead+len(p): WrapInPlace дополняет payload до padTarget
	// байт + 2B маркер (см. rtpopus3.go), сырая формула была бы мала для
	// коротких пакетов.
	wireLen := c.conn.MaxWire(len(p))

	bp := bufPool.Get().(*[]byte) //nolint:errcheck // pool New always returns *[]byte
	out := *bp
	if cap(out) < wireLen {
		out = make([]byte, wireLen)
		*bp = out
	}
	out = out[:wireLen]
	defer bufPool.Put(bp)

	n, err := c.conn.WrapInto(out, p)
	if err != nil {
		return 0, err
	}
	if _, err := c.inner.WriteTo(out[:n], addr); err != nil {
		return 0, err
	}
	return len(p), nil
}

func (c *packetConn) Close() error                       { return c.inner.Close() }
func (c *packetConn) LocalAddr() net.Addr                { return c.inner.LocalAddr() }
func (c *packetConn) SetDeadline(t time.Time) error      { return c.inner.SetDeadline(t) }
func (c *packetConn) SetReadDeadline(t time.Time) error  { return c.inner.SetReadDeadline(t) }
func (c *packetConn) SetWriteDeadline(t time.Time) error { return c.inner.SetWriteDeadline(t) }
