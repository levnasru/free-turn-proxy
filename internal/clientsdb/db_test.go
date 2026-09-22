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

func TestBaseClientID(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"client123", "client123"},
		{"client123#device1", "client123"},
		{"client123#phone-uuid-456", "client123"},
		{"client123@pc", "client123"},
		{"client123/mac", "client123"},
		{"", ""},
	}
	for _, tc := range tests {
		if got := BaseClientID(tc.input); got != tc.want {
			t.Errorf("BaseClientID(%q) = %q, want %q", tc.input, got, tc.want)
		}
	}
}

func TestClientsDBDeviceSuffix(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "clients.json")

	db, err := New(dbPath)
	if err != nil {
		t.Fatalf("Failed to create db: %v", err)
	}

	if err = db.Add("user-alpha", "Test User Alpha", 2); err != nil {
		t.Fatalf("Failed to add client: %v", err)
	}

	// Base ID is authorized
	if !db.IsAuthorized("user-alpha") {
		t.Errorf("Expected base user-alpha to be authorized")
	}

	// Device-suffixed IDs are authorized via BaseClientID
	if !db.IsAuthorized("user-alpha#phone-1") {
		t.Errorf("Expected user-alpha#phone-1 to be authorized")
	}
	if !db.IsAuthorized("user-alpha#desktop-2") {
		t.Errorf("Expected user-alpha#desktop-2 to be authorized")
	}
	if db.IsAuthorized("user-beta#phone-1") {
		t.Errorf("Expected user-beta#phone-1 to NOT be authorized")
	}

	// Quota is tracked against base ID across devices
	if !db.TryAcquireStream("user-alpha#phone-1") {
		t.Fatalf("phone-1 acquire stream failed")
	}
	if !db.TryAcquireStream("user-alpha#desktop-2") {
		t.Fatalf("desktop-2 acquire stream failed")
	}
	// MaxStreams is 2, third connection from any device should fail
	if db.TryAcquireStream("user-alpha#tablet-3") {
		t.Fatalf("expected tablet-3 acquire to be refused due to max_streams=2")
	}

	// Release from one device allows another to acquire
	db.ReleaseStream("user-alpha#phone-1")
	if !db.TryAcquireStream("user-alpha#tablet-3") {
		t.Fatalf("tablet-3 acquire failed after phone-1 release")
	}
}
