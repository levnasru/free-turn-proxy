package rtpvideo

import (
	"bytes"
	"crypto/rand"
	"testing"

	"github.com/samosvalishe/free-turn-proxy/internal/wire/rtpopus3"
)

func TestRTPVideoRoundtrip(t *testing.T) {
	key := make([]byte, KeyLen)
	_, _ = rand.Read(key)

	client, err := NewConn(key, false)
	if err != nil {
		t.Fatalf("client: %v", err)
	}
	server, err := NewConn(key, true)
	if err != nil {
		t.Fatalf("server: %v", err)
	}

	payload := []byte("hello-webrtc-vp8-video-payload-test-12345")
	buf := make([]byte, 1600)
	copy(buf[client.HeaderLen():], payload)

	wireLen, err := client.WrapInPlace(buf, len(payload))
	if err != nil {
		t.Fatalf("wrap: %v", err)
	}

	wire := buf[:wireLen]
	// Verify RTP header
	if wire[0] != 0x90 {
		t.Fatalf("byte 0: got 0x%x, want 0x90", wire[0])
	}
	if wire[1]&0x7F != rtpPTVideo {
		t.Fatalf("PT: got 0x%x, want 0x60 (VP8 video)", wire[1]&0x7F)
	}
	// Verify RFC 8285 extension
	if wire[12] != 0xBE || wire[13] != 0xDE {
		t.Fatalf("extension marker: got %x %x, want 0xBE 0xDE", wire[12], wire[13])
	}
	// Verify mid is '1' (video)
	if wire[24] != midVideoValue {
		t.Fatalf("mid value: got %c, want %c", wire[24], midVideoValue)
	}
	// Verify VP8 descriptor is 0x10
	if wire[28] != vp8DescKeyframe {
		t.Fatalf("vp8 descriptor: got 0x%x, want 0x10", wire[28])
	}

	// Server unwrap
	plain, err := server.UnwrapInPlace(wire)
	if err != nil {
		t.Fatalf("unwrap: %v", err)
	}
	if !bytes.Equal(plain, payload) {
		t.Fatalf("decrypted payload mismatch:\ngot:  %s\nwant: %s", string(plain), string(payload))
	}
}

func TestRTPVideoOpusBackwardCompatibility(t *testing.T) {
	key := make([]byte, KeyLen)
	_, _ = rand.Read(key)

	// Simulated old client using rtpopus3
	oldClient, err := rtpopus3.NewConn(key, false)
	if err != nil {
		t.Fatalf("old client: %v", err)
	}

	// New server using rtpvideo
	newServer, err := NewConn(key, true)
	if err != nil {
		t.Fatalf("new server: %v", err)
	}

	// 1. Old client sends Opus packet
	clientMsg := []byte("opus-client-message")
	cBuf := make([]byte, 1600)
	copy(cBuf[oldClient.HeaderLen():], clientMsg)

	cWireLen, err := oldClient.WrapInPlace(cBuf, len(clientMsg))
	if err != nil {
		t.Fatalf("oldClient wrap: %v", err)
	}
	cWire := cBuf[:cWireLen]

	// Verify it was sent as Opus (PT=111)
	if cWire[1]&0x7F != 0x6F {
		t.Fatalf("old client PT: got 0x%x, want 0x6F", cWire[1]&0x7F)
	}

	// 2. New server unwraps the old client's packet
	serverReceived, err := newServer.UnwrapInPlace(cWire)
	if err != nil {
		t.Fatalf("newServer unwrap of opus packet failed: %v", err)
	}
	if !bytes.Equal(serverReceived, clientMsg) {
		t.Fatalf("server received payload mismatch: %s != %s", serverReceived, clientMsg)
	}

	// 3. New server replies to the old client
	replyMsg := []byte("server-response-to-opus")
	sBuf := make([]byte, 1600)
	copy(sBuf[newServer.HeaderLen():], replyMsg)

	sWireLen, err := newServer.WrapInPlace(sBuf, len(replyMsg))
	if err != nil {
		t.Fatalf("newServer wrap: %v", err)
	}
	sWire := sBuf[:sWireLen]

	// The server must reply in Opus format (PT=111) to not break the old client!
	if sWire[1]&0x7F != 0x6F {
		t.Fatalf("newServer reply PT: got 0x%x, want 0x6F (must match client)", sWire[1]&0x7F)
	}

	// 4. Old client unwraps the server's reply
	clientReceived, err := oldClient.UnwrapInPlace(sWire)
	if err != nil {
		t.Fatalf("oldClient unwrap of server response failed: %v", err)
	}
	if !bytes.Equal(clientReceived, replyMsg) {
		t.Fatalf("client received payload mismatch: %s != %s", clientReceived, replyMsg)
	}
}

func TestRTPVideoToRTPOpus3ServerAndBack(t *testing.T) {
	key := make([]byte, KeyLen)
	_, _ = rand.Read(key)

	cli, err := NewConn(key, false)
	if err != nil {
		t.Fatalf("cli: %v", err)
	}
	srv, err := rtpopus3.NewConn(key, true)
	if err != nil {
		t.Fatalf("srv: %v", err)
	}

	cliPayload := []byte("client-vp8-video-message-12345")
	cliBuf := make([]byte, cli.MaxWire(len(cliPayload)))
	copy(cliBuf[cli.HeaderLen():], cliPayload)

	cn, err := cli.WrapInPlace(cliBuf, len(cliPayload))
	if err != nil {
		t.Fatalf("cli wrap: %v", err)
	}

	srvPlain, err := srv.UnwrapInPlace(cliBuf[:cn])
	if err != nil {
		t.Fatalf("srv unwrap: %v", err)
	}
	if !bytes.Equal(srvPlain, cliPayload) {
		t.Fatalf("srv plain mismatch: %s != %s", srvPlain, cliPayload)
	}
	if !srv.IsVideo() {
		t.Fatal("srv must be marked as IsVideo()")
	}

	srvReply := []byte("server-response-to-vp8-67890")
	srvBuf := make([]byte, srv.MaxWire(len(srvReply)))
	copy(srvBuf[srv.HeaderLen():], srvReply)

	sn, err := srv.WrapInPlace(srvBuf, len(srvReply))
	if err != nil {
		t.Fatalf("srv wrap: %v", err)
	}

	cliPlain, err := cli.UnwrapInPlace(srvBuf[:sn])
	if err != nil {
		t.Fatalf("cli unwrap: %v", err)
	}
	if !bytes.Equal(cliPlain, srvReply) {
		t.Fatalf("cli plain mismatch: %s != %s", cliPlain, srvReply)
	}
}
