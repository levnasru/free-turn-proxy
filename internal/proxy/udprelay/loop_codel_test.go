package udprelay

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pion/dtls/v3"
	"github.com/pion/turn/v5"
	"github.com/samosvalishe/free-turn-proxy/internal/clientsdb"
	"github.com/samosvalishe/free-turn-proxy/internal/logx"
	"github.com/samosvalishe/free-turn-proxy/internal/transport/dtlsdial"
	"github.com/samosvalishe/free-turn-proxy/internal/wire/codel"
)

// TestDTLS_Over_CoDelPipe verifies that pion/dtls handshake and bi-directional
// encrypted communication execute cleanly over codel.Pipe.
func TestDTLS_Over_CoDelPipe(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// 1. Create CoDel pipe: conn1 (client side) <-> conn2 (server side)
	conn1, conn2 := codel.NewPipe(30, 30)
	defer conn1.Close()
	defer conn2.Close()

	fakePeer := &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 12345}

	// 2. Server DTLS setup
	serverCert, err := dtlsdial.GenerateSelfSignedCert()
	if err != nil {
		t.Fatalf("generate server cert: %v", err)
	}

	serverConfig := &dtls.Config{
		Certificates:           []tls.Certificate{serverCert},
		ExtendedMasterSecret:   dtls.RequireExtendedMasterSecret,
		CipherSuites:           []dtls.CipherSuiteID{dtls.TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256},
		ConnectionIDGenerator:  dtls.RandomCIDGenerator(8),
		ReplayProtectionWindow: dtlsdial.DefaultReplayProtectionWindow,
	}

	serverDone := make(chan struct{})
	var serverDTLSConn *dtls.Conn
	var serverErr error

	go func() {
		defer close(serverDone)
		// Server side handshake over conn2
		serverDTLSConn, serverErr = dtls.Server(conn2, fakePeer, serverConfig)
		if serverErr != nil {
			return
		}
		serverErr = serverDTLSConn.HandshakeContext(ctx)
	}()

	// 3. Client DTLS setup via dtlsdial.Dialer over conn1
	dialer := &dtlsdial.Dialer{HandshakeTimeout: 5 * time.Second}
	clientDTLSConn, err := dialer.Dial(ctx, conn1, fakePeer)
	if err != nil {
		t.Fatalf("client dial failed: %v", err)
	}
	defer clientDTLSConn.Close()

	<-serverDone
	if serverErr != nil {
		t.Fatalf("server handshake failed: %v", serverErr)
	}
	defer serverDTLSConn.Close()

	// 4. Test client ID write/read (same as in udprelay and cmd/server)
	const testClientID = "test-client-12345"
	go func() {
		if err := clientsdb.WriteClientID(clientDTLSConn, testClientID); err != nil {
			t.Errorf("write client ID: %v", err)
		}
	}()

	readID, err := clientsdb.ReadClientID(serverDTLSConn)
	if err != nil {
		t.Fatalf("read client ID: %v", err)
	}
	if readID != testClientID {
		t.Fatalf("client ID mismatch: got %q, want %q", readID, testClientID)
	}

	// 5. Test bi-directional ping-pong
	msgFromClient := []byte("hello from wireguard client through codel pipe")
	if _, err := clientDTLSConn.Write(msgFromClient); err != nil {
		t.Fatalf("client write: %v", err)
	}

	buf := make([]byte, 1024)
	n, err := serverDTLSConn.Read(buf)
	if err != nil {
		t.Fatalf("server read: %v", err)
	}
	if string(buf[:n]) != string(msgFromClient) {
		t.Fatalf("payload mismatch: got %q, want %q", string(buf[:n]), string(msgFromClient))
	}

	// Server reply
	msgFromServer := []byte("reply from remote server through codel pipe")
	if _, err := serverDTLSConn.Write(msgFromServer); err != nil {
		t.Fatalf("server write: %v", err)
	}

	n, err = clientDTLSConn.Read(buf)
	if err != nil {
		t.Fatalf("client read: %v", err)
	}
	if string(buf[:n]) != string(msgFromServer) {
		t.Fatalf("reply mismatch: got %q, want %q", string(buf[:n]), string(msgFromServer))
	}
}

// TestDTLS_CoDelPipe_PacedDrainAndBurst verifies that under simulated 5ms
// pacing (representing -obf-timing), rapid bursts of packets are tail-dropped
// properly, delivered packets maintain strictly monotonic order, and no head-drop occurs.
func TestDTLS_CoDelPipe_PacedDrainAndBurst(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	conn1, conn2 := codel.NewPipe(20, 20)
	defer conn1.Close()
	defer conn2.Close()

	fakePeer := &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 12345}

	serverCert, err := dtlsdial.GenerateSelfSignedCert()
	if err != nil {
		t.Fatalf("generate cert: %v", err)
	}
	serverConfig := &dtls.Config{
		Certificates:           []tls.Certificate{serverCert},
		ExtendedMasterSecret:   dtls.RequireExtendedMasterSecret,
		CipherSuites:           []dtls.CipherSuiteID{dtls.TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256},
		ConnectionIDGenerator:  dtls.RandomCIDGenerator(8),
		ReplayProtectionWindow: dtlsdial.DefaultReplayProtectionWindow,
	}

	var serverDTLSConn *dtls.Conn
	var serverErr error
	serverDone := make(chan struct{})
	go func() {
		defer close(serverDone)
		serverDTLSConn, serverErr = dtls.Server(conn2, fakePeer, serverConfig)
		if serverErr != nil {
			return
		}
		serverErr = serverDTLSConn.HandshakeContext(ctx)
	}()

	dialer := &dtlsdial.Dialer{HandshakeTimeout: 5 * time.Second}
	clientDTLSConn, err := dialer.Dial(ctx, conn1, fakePeer)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer clientDTLSConn.Close()

	<-serverDone
	if serverErr != nil {
		t.Fatalf("handshake: %v", serverErr)
	}
	defer serverDTLSConn.Close()

	// Drain goroutine reading from serverDTLSConn with simulated pacing (5ms per packet)
	const totalPackets = 100
	receivedSeqs := make([]int, 0, totalPackets)
	var recvMu sync.Mutex
	drainDone := make(chan struct{})

	go func() {
		defer close(drainDone)
		buf := make([]byte, 2048)
		for {
			_ = serverDTLSConn.SetReadDeadline(time.Now().Add(200 * time.Millisecond))
			n, err := serverDTLSConn.Read(buf)
			if err != nil {
				return
			}
			var seq int
			if _, err := fmt.Sscanf(string(buf[:n]), "pkt-%d", &seq); err == nil {
				recvMu.Lock()
				receivedSeqs = append(receivedSeqs, seq)
				recvMu.Unlock()
			}
			// Simulated pacing delay
			time.Sleep(5 * time.Millisecond)
		}
	}()

	// Send burst: 100 packets sent in 20ms (much faster than 5ms drain rate)
	for i := 0; i < totalPackets; i++ {
		payload := fmt.Sprintf("pkt-%d", i)
		_, _ = clientDTLSConn.Write([]byte(payload))
		time.Sleep(200 * time.Microsecond)
	}

	<-drainDone

	recvMu.Lock()
	count := len(receivedSeqs)
	seqs := make([]int, count)
	copy(seqs, receivedSeqs)
	recvMu.Unlock()

	t.Logf("Burst test: sent %d packets, received %d packets", totalPackets, count)

	if count == 0 {
		t.Fatal("expected to receive packets, got 0")
	}

	// Crucial check 1: FIRST packet must be pkt-0!
	// (Under the old head-drop bug, pkt-0 was evicted by later packets).
	if seqs[0] != 0 {
		t.Fatalf("HEAD DROP DETECTED! First packet received was %d, expected 0", seqs[0])
	}

	// Crucial check 2: All received packets MUST be strictly monotonically increasing!
	for i := 1; i < len(seqs); i++ {
		if seqs[i] <= seqs[i-1] {
			t.Fatalf("SEQUENCE CORRUPTION! seq[%d]=%d <= seq[%d]=%d", i, seqs[i], i-1, seqs[i-1])
		}
	}
}

// mockAuth satisfies AuthHandler for udprelay.
type mockAuth struct{}

func (mockAuth) IsAuthError(err error) bool       { return false }
func (mockAuth) HandleAuthError(streamID int) bool { return false }
func (mockAuth) ResetErrors(streamID int)          {}
func (mockAuth) BackoffUntilUnix() int64           { return 0 }

// TestEndToEnd_WithLocalTURN_And_CoDel runs a complete local end-to-end loop:
// Mock WG Client -> udprelay.Run (with CoDel Pipe) -> Local Pion TURN -> Local DTLS Server -> Echo Server
// and back.
func TestEndToEnd_WithLocalTURN_And_CoDel(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	logger := logx.New(false)

	// 1. Start local UDP Echo Server (backend target)
	echoConn, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("echo server listen: %v", err)
	}
	defer echoConn.Close()
	echoAddr := echoConn.LocalAddr().String()

	go func() {
		buf := make([]byte, 2048)
		for {
			n, from, err := echoConn.ReadFrom(buf)
			if err != nil {
				return
			}
			_, _ = echoConn.WriteTo(buf[:n], from)
		}
	}()

	// 2. Start local Pion TURN server on 127.0.0.1
	turnConn, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("turn listen: %v", err)
	}
	defer turnConn.Close()
	turnPort := fmt.Sprintf("%d", turnConn.LocalAddr().(*net.UDPAddr).Port)

	const turnUser = "testuser"
	const turnPass = "testpass"
	const turnRealm = "testrealm"

	turnServer, err := turn.NewServer(turn.ServerConfig{
		Realm: turnRealm,
		AuthHandler: func(ra *turn.RequestAttributes) (string, []byte, bool) {
			if ra.Username == turnUser {
				return turnUser, turn.GenerateAuthKey(turnUser, turnRealm, turnPass), true
			}
			return "", nil, false
		},
		PacketConnConfigs: []turn.PacketConnConfig{
			{
				PacketConn: turnConn,
				RelayAddressGenerator: &turn.RelayAddressGeneratorStatic{
					RelayAddress: net.ParseIP("127.0.0.1"),
					Address:      "127.0.0.1",
				},
			},
		},
	})
	if err != nil {
		t.Fatalf("pion turn server: %v", err)
	}
	defer turnServer.Close()

	// 3. Start local DTLS server (simulating cmd/server)
	serverAddr, err := net.ResolveUDPAddr("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}

	serverCert, err := dtlsdial.GenerateSelfSignedCert()
	if err != nil {
		t.Fatalf("server cert: %v", err)
	}
	serverDTLSOpts := []dtls.ServerOption{
		dtls.WithCertificates(serverCert),
		dtls.WithExtendedMasterSecret(dtls.RequireExtendedMasterSecret),
		dtls.WithCipherSuites(dtls.TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256),
		dtls.WithConnectionIDGenerator(dtls.RandomCIDGenerator(8)),
		dtls.WithReplayProtectionWindow(dtlsdial.DefaultReplayProtectionWindow),
	}
	dtlsListener, err := dtls.ListenWithOptions("udp", serverAddr, serverDTLSOpts...)
	if err != nil {
		t.Fatalf("dtls listener: %v", err)
	}
	defer dtlsListener.Close()
	boundServerAddr := dtlsListener.Addr().(*net.UDPAddr)

	// DTLS server accept loop: read client ID, handle UDP forwarding to echo server
	go func() {
		for {
			conn, err := dtlsListener.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				// Read client ID
				if _, err := clientsdb.ReadClientID(c); err != nil {
					return
				}
				// Forward packets between DTLS conn and echo server
				targetConn, err := net.Dial("udp", echoAddr)
				if err != nil {
					return
				}
				defer targetConn.Close()

				var wg sync.WaitGroup
				wg.Add(2)
				go func() {
					defer wg.Done()
					b := make([]byte, 2048)
					for {
						_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
						n, err := c.Read(b)
						if err != nil {
							return
						}
						if _, err := targetConn.Write(b[:n]); err != nil {
							return
						}
					}
				}()
				go func() {
					defer wg.Done()
					b := make([]byte, 2048)
					for {
						_ = targetConn.SetReadDeadline(time.Now().Add(5 * time.Second))
						n, err := targetConn.Read(b)
						if err != nil {
							return
						}
						if _, err := c.Write(b[:n]); err != nil {
							return
						}
					}
				}()
				wg.Wait()
			}(conn)
		}
	}()

	// 4. Start udprelay.Run
	clientListenAddr := "127.0.0.1:0"
	tmpListen, err := net.ListenPacket("udp", clientListenAddr)
	if err != nil {
		t.Fatalf("tmp listen: %v", err)
	}
	clientListenPort := tmpListen.LocalAddr().String()
	_ = tmpListen.Close()

	dtlsDialer := &dtlsdial.Dialer{HandshakeTimeout: 5 * time.Second}
	var connectedStreams atomic.Int32

	params := &Params{
		Host:         "127.0.0.1",
		Port:         turnPort,
		TransportUDP: true,
		Profile:      "none",
		ClientID:     "smoke-test-client",
		GetCreds: func(ctx context.Context, streamID int) (string, string, []string, error) {
			return turnUser, turnPass, []string{net.JoinHostPort("127.0.0.1", turnPort)}, nil
		},
	}

	relayCtx, relayCancel := context.WithCancel(ctx)
	defer relayCancel()

	runErrCh := make(chan error, 1)
	go func() {
		runErrCh <- Run(relayCtx, dtlsDialer, mockAuth{}, logger, &connectedStreams, params, boundServerAddr, clientListenPort, 1)
	}()

	// Wait for connected stream
	streamReady := false
	for i := 0; i < 50; i++ {
		if connectedStreams.Load() >= 1 {
			streamReady = true
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if !streamReady {
		t.Fatal("timed out waiting for udprelay stream to connect")
	}

	// 5. Mock WG client sending packets through udprelay
	wgClient, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("wg client listen: %v", err)
	}
	defer wgClient.Close()

	proxyUDPAddr, err := net.ResolveUDPAddr("udp", clientListenPort)
	if err != nil {
		t.Fatalf("resolve proxy addr: %v", err)
	}

	// Send 10 packets and verify round-trip
	for i := 0; i < 10; i++ {
		msg := fmt.Sprintf("wireguard-packet-%d", i)
		if _, err := wgClient.WriteTo([]byte(msg), proxyUDPAddr); err != nil {
			t.Fatalf("write to proxy: %v", err)
		}

		_ = wgClient.SetReadDeadline(time.Now().Add(2 * time.Second))
		buf := make([]byte, 2048)
		n, from, err := wgClient.ReadFrom(buf)
		if err != nil {
			t.Fatalf("packet %d round trip failed: %v", i, err)
		}
		if string(buf[:n]) != msg {
			t.Fatalf("packet %d data mismatch: got %q, want %q", i, string(buf[:n]), msg)
		}
		_ = from
	}

	t.Log("Successfully sent and received 10 packets through complete loopback stack (WG -> CoDel -> DTLS -> TURN -> DTLS Server -> Echo)!")
}

// TestEndToEnd_WithPacing7ms_And_Burst verifies the exact production configuration:
// ObfTiming=7ms with CoDel queue under burst traffic.
func TestEndToEnd_WithPacing7ms_And_Burst(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	logger := logx.New(false)

	// 1. Backend Echo Server
	echoConn, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("echo server listen: %v", err)
	}
	defer echoConn.Close()
	echoAddr := echoConn.LocalAddr().String()

	go func() {
		buf := make([]byte, 2048)
		for {
			n, from, err := echoConn.ReadFrom(buf)
			if err != nil {
				return
			}
			_, _ = echoConn.WriteTo(buf[:n], from)
		}
	}()

	// 2. TURN Server
	turnConn, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("turn listen: %v", err)
	}
	defer turnConn.Close()
	turnPort := fmt.Sprintf("%d", turnConn.LocalAddr().(*net.UDPAddr).Port)

	const turnUser = "testuser7ms"
	const turnPass = "testpass7ms"
	const turnRealm = "testrealm7ms"

	turnServer, err := turn.NewServer(turn.ServerConfig{
		Realm: turnRealm,
		AuthHandler: func(ra *turn.RequestAttributes) (string, []byte, bool) {
			if ra.Username == turnUser {
				return turnUser, turn.GenerateAuthKey(turnUser, turnRealm, turnPass), true
			}
			return "", nil, false
		},
		PacketConnConfigs: []turn.PacketConnConfig{
			{
				PacketConn: turnConn,
				RelayAddressGenerator: &turn.RelayAddressGeneratorStatic{
					RelayAddress: net.ParseIP("127.0.0.1"),
					Address:      "127.0.0.1",
				},
			},
		},
	})
	if err != nil {
		t.Fatalf("pion turn server: %v", err)
	}
	defer turnServer.Close()

	// 3. DTLS Server
	serverAddr, err := net.ResolveUDPAddr("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}

	serverCert, err := dtlsdial.GenerateSelfSignedCert()
	if err != nil {
		t.Fatalf("server cert: %v", err)
	}
	serverDTLSOpts := []dtls.ServerOption{
		dtls.WithCertificates(serverCert),
		dtls.WithExtendedMasterSecret(dtls.RequireExtendedMasterSecret),
		dtls.WithCipherSuites(dtls.TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256),
		dtls.WithConnectionIDGenerator(dtls.RandomCIDGenerator(8)),
		dtls.WithReplayProtectionWindow(dtlsdial.DefaultReplayProtectionWindow),
	}
	dtlsListener, err := dtls.ListenWithOptions("udp", serverAddr, serverDTLSOpts...)
	if err != nil {
		t.Fatalf("dtls listener: %v", err)
	}
	defer dtlsListener.Close()
	boundServerAddr := dtlsListener.Addr().(*net.UDPAddr)

	go func() {
		for {
			conn, err := dtlsListener.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				if _, err := clientsdb.ReadClientID(c); err != nil {
					return
				}
				targetConn, err := net.Dial("udp", echoAddr)
				if err != nil {
					return
				}
				defer targetConn.Close()

				var wg sync.WaitGroup
				wg.Add(2)
				go func() {
					defer wg.Done()
					b := make([]byte, 2048)
					for {
						_ = c.SetReadDeadline(time.Now().Add(10 * time.Second))
						n, err := c.Read(b)
						if err != nil {
							return
						}
						if _, err := targetConn.Write(b[:n]); err != nil {
							return
						}
					}
				}()
				go func() {
					defer wg.Done()
					b := make([]byte, 2048)
					for {
						_ = targetConn.SetReadDeadline(time.Now().Add(10 * time.Second))
						n, err := targetConn.Read(b)
						if err != nil {
							return
						}
						if _, err := c.Write(b[:n]); err != nil {
							return
						}
					}
				}()
				wg.Wait()
			}(conn)
		}
	}()

	// 4. Client udprelay with ObfTiming = 7ms
	tmpListen, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("tmp listen: %v", err)
	}
	clientListenPort := tmpListen.LocalAddr().String()
	_ = tmpListen.Close()

	dtlsDialer := &dtlsdial.Dialer{HandshakeTimeout: 5 * time.Second}
	var connectedStreams atomic.Int32

	params := &Params{
		Host:         "127.0.0.1",
		Port:         turnPort,
		TransportUDP: true,
		Profile:      "none",
		ObfTiming:    7 * time.Millisecond, // Exact production timing flag!
		ClientID:     "smoke-test-7ms",
		GetCreds: func(ctx context.Context, streamID int) (string, string, []string, error) {
			return turnUser, turnPass, []string{net.JoinHostPort("127.0.0.1", turnPort)}, nil
		},
	}

	relayCtx, relayCancel := context.WithCancel(ctx)
	defer relayCancel()

	go func() {
		_ = Run(relayCtx, dtlsDialer, mockAuth{}, logger, &connectedStreams, params, boundServerAddr, clientListenPort, 1)
	}()

	for i := 0; i < 50; i++ {
		if connectedStreams.Load() >= 1 {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if connectedStreams.Load() < 1 {
		t.Fatal("timed out waiting for stream to connect")
	}

	// 5. Send burst through proxy
	wgClient, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("wg client listen: %v", err)
	}
	defer wgClient.Close()

	proxyUDPAddr, err := net.ResolveUDPAddr("udp", clientListenPort)
	if err != nil {
		t.Fatalf("resolve proxy addr: %v", err)
	}

	// Send 30 packets rapidly
	const burstCount = 30
	for i := 0; i < burstCount; i++ {
		msg := fmt.Sprintf("pacing-pkt-%02d", i)
		if _, err := wgClient.WriteTo([]byte(msg), proxyUDPAddr); err != nil {
			t.Fatalf("write %d: %v", i, err)
		}
		time.Sleep(500 * time.Microsecond)
	}

	// Receive packets back
	received := make([]int, 0, burstCount)
	deadline := time.Now().Add(3 * time.Second)
	buf := make([]byte, 2048)
	for time.Now().Before(deadline) {
		_ = wgClient.SetReadDeadline(time.Now().Add(200 * time.Millisecond))
		n, _, err := wgClient.ReadFrom(buf)
		if err != nil {
			break
		}
		var seq int
		if _, err := fmt.Sscanf(string(buf[:n]), "pacing-pkt-%02d", &seq); err == nil {
			received = append(received, seq)
		}
	}

	t.Logf("Pacing 7ms test: sent %d packets, received %d echoed back", burstCount, len(received))
	if len(received) == 0 {
		t.Fatal("expected echoed packets, got 0")
	}

	// Verify monotonic order (no reordering or head corruption)
	if received[0] != 0 {
		t.Fatalf("Head drop detected! First received was %d, expected 0", received[0])
	}
	for i := 1; i < len(received); i++ {
		if received[i] <= received[i-1] {
			t.Fatalf("Sequence inversion: seq[%d]=%d <= seq[%d]=%d", i, received[i], i-1, received[i-1])
		}
	}
	t.Logf("Strict sequence order maintained across 7ms paced relay!")
}

