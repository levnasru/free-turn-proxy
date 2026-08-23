package clientsdb

import (
	"net"
	"path/filepath"
	"testing"
	"time"
)

func TestClientsDB(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "clients.json")

	db, err := New(dbPath)
	if err != nil {
		t.Fatalf("Failed to create db: %v", err)
	}

	if err = db.Add("client-123", "Test 1", 0); err != nil {
		t.Fatalf("Failed to add client: %v", err)
	}

	if !db.IsAuthorized("client-123") {
		t.Errorf("Expected client-123 to be authorized")
	}

	if db.IsAuthorized("client-456") {
		t.Errorf("Expected client-456 not to be authorized")
	}

	if err = db.Remove("client-123"); err != nil {
		t.Fatalf("Failed to remove client: %v", err)
	}

	if db.IsAuthorized("client-123") {
		t.Errorf("Expected client-123 to be removed")
	}

	// Test persistence
	_ = db.Add("client-789", "Test Persistence", 0)

	db2, err := New(dbPath)
	if err != nil {
		t.Fatalf("Failed to create db2: %v", err)
	}

	if !db2.IsAuthorized("client-789") {
		t.Errorf("Expected client-789 to be persisted")
	}

	// Test hot reload manually
	db2.mu.Lock()
	db2.lastModified = db2.lastModified.Add(-1 * time.Second)
	db2.mu.Unlock()

	_ = db.Add("client-999", "Hot reload test", 0)
	db2.loadIfModified()

	if !db2.IsAuthorized("client-999") {
		t.Errorf("Expected client-999 to be loaded via hot reload")
	}
}

func TestClientsDBMaxStreams(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "clients.json")

	db, err := New(dbPath)
	if err != nil {
		t.Fatalf("Failed to create db: %v", err)
	}

	if err = db.Add("capped", "N=2", 2); err != nil {
		t.Fatalf("Failed to add client: %v", err)
	}
	if err = db.Add("unlimited", "N=0", 0); err != nil {
		t.Fatalf("Failed to add client: %v", err)
	}

	// A client outside the db can never acquire, no matter what it asks for.
	if db.TryAcquireStream("ghost") {
		t.Errorf("Unauthorized client acquired a stream slot")
	}

	// capped=2 grants exactly two concurrent slots, then refuses further
	// connections regardless of how many the client itself tries to open -
	// this is the server-side enforcement, independent of the client's -n.
	if !db.TryAcquireStream("capped") {
		t.Fatalf("Expected first acquire to succeed")
	}
	if !db.TryAcquireStream("capped") {
		t.Fatalf("Expected second acquire to succeed")
	}
	if db.TryAcquireStream("capped") {
		t.Errorf("Expected third acquire to be refused (max_streams=2)")
	}

	// Releasing frees a slot back up.
	db.ReleaseStream("capped")
	if !db.TryAcquireStream("capped") {
		t.Errorf("Expected acquire to succeed after release")
	}

	// max_streams=0 means unlimited.
	for i := 0; i < 50; i++ {
		if !db.TryAcquireStream("unlimited") {
			t.Fatalf("Expected unlimited client to always acquire (i=%d)", i)
		}
	}
}

func TestClientIDRoundTrip(t *testing.T) {
	addr := &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 0}
	serverConn, err := net.ListenUDP("udp", addr)
	if err != nil {
		t.Fatalf("ListenUDP: %v", err)
	}
	defer func() { _ = serverConn.Close() }()

	udpAddr, ok := serverConn.LocalAddr().(*net.UDPAddr)
	if !ok {
		t.Fatalf("LocalAddr is not *net.UDPAddr")
	}
	clientConn, err := net.DialUDP("udp", nil, udpAddr)
	if err != nil {
		t.Fatalf("DialUDP: %v", err)
	}
	defer func() { _ = clientConn.Close() }()

	expectedID := "client-test-uuid-123"

	// Client writes
	go func() {
		if werr := WriteClientID(clientConn, expectedID); werr != nil {
			t.Errorf("WriteClientID failed: %v", werr)
		}
	}()

	// Server reads
	readID, err := ReadClientID(serverConn)
	if err != nil {
		t.Fatalf("ReadClientID: %v", err)
	}

	if readID != expectedID {
		t.Errorf("expected %q, got %q", expectedID, readID)
	}
}
