//go:build unix

package mobile

import (
	"net"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

func TestSendFD_AckSuccess(t *testing.T) {
	dir := t.TempDir()
	sockPath := filepath.Join(dir, "protect.sock")

	l, err := net.ListenUnix("unix", &net.UnixAddr{Name: sockPath, Net: "unix"})
	if err != nil {
		t.Fatalf("ListenUnix: %v", err)
	}
	defer l.Close()

	done := make(chan struct{})
	go func() {
		defer close(done)
		conn, err := l.AcceptUnix()
		if err != nil {
			t.Errorf("AcceptUnix: %v", err)
			return
		}
		defer conn.Close()

		buf := make([]byte, 32)
		oob := make([]byte, 128)
		_, _, _, _, err = conn.ReadMsgUnix(buf, oob)
		if err != nil {
			t.Errorf("ReadMsgUnix: %v", err)
			return
		}

		// Write 1-byte ack
		_, err = conn.Write([]byte{1})
		if err != nil {
			t.Errorf("conn.Write ack: %v", err)
		}
	}()

	dummyFile, err := os.CreateTemp(dir, "dummy")
	if err != nil {
		t.Fatalf("CreateTemp: %v", err)
	}
	defer dummyFile.Close()

	err = SendFD(sockPath, int(dummyFile.Fd()))
	if err != nil {
		t.Fatalf("SendFD failed: %v", err)
	}
	<-done
}

func TestSendFD_AckTimeoutOrClosed(t *testing.T) {
	dir := t.TempDir()
	sockPath := filepath.Join(dir, "protect.sock")

	l, err := net.ListenUnix("unix", &net.UnixAddr{Name: sockPath, Net: "unix"})
	if err != nil {
		t.Fatalf("ListenUnix: %v", err)
	}
	defer l.Close()

	go func() {
		conn, err := l.AcceptUnix()
		if err != nil {
			return
		}
		// Close immediately without writing ack
		conn.Close()
	}()

	err = SendFD(sockPath, int(syscall.Stdin))
	if err == nil {
		t.Fatalf("expected SendFD to fail when connection is closed without ack")
	}
}
