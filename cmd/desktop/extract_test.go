package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestExtractIfChangedWritesNewFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sub", "client")
	data := []byte("fake binary content v1")

	if err := extractIfChanged(path, data, 0o755); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading extracted file: %v", err)
	}
	if string(got) != string(data) {
		t.Fatalf("content = %q, want %q", got, data)
	}

	if _, err := os.ReadFile(path + ".sha256"); err != nil {
		t.Fatalf("expected sidecar hash file, got error: %v", err)
	}
}

func TestExtractIfChangedSkipsUnchangedContent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "client")
	data := []byte("fake binary content v1")

	if err := extractIfChanged(path, data, 0o755); err != nil {
		t.Fatalf("first extract: %v", err)
	}
	first, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat after first extract: %v", err)
	}

	// Force a distinguishable mtime so a real rewrite would be detectable —
	// otherwise two writes fast enough could land on the same mtime and the
	// test would pass even if extractIfChanged wrongly rewrote.
	past := time.Now().Add(-time.Hour)
	if err := os.Chtimes(path, past, past); err != nil {
		t.Fatalf("chtimes: %v", err)
	}

	if err := extractIfChanged(path, data, 0o755); err != nil {
		t.Fatalf("second extract: %v", err)
	}
	second, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat after second extract: %v", err)
	}
	if !second.ModTime().Equal(past) {
		t.Fatalf("file was rewritten on unchanged content: mtime = %v, want unchanged %v (original before chtimes: %v)", second.ModTime(), past, first.ModTime())
	}
}

func TestExtractIfChangedRewritesOnDifferentContent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "client")

	if err := extractIfChanged(path, []byte("content v1"), 0o755); err != nil {
		t.Fatalf("first extract: %v", err)
	}
	if err := extractIfChanged(path, []byte("content v2, longer than v1"), 0o755); err != nil {
		t.Fatalf("second extract: %v", err)
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading extracted file: %v", err)
	}
	if string(got) != "content v2, longer than v1" {
		t.Fatalf("content = %q, want updated content", got)
	}
}

func TestExtractIfChangedRewritesWhenFileMissingButSidecarPresent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "client")
	data := []byte("fake binary content")

	if err := extractIfChanged(path, data, 0o755); err != nil {
		t.Fatalf("first extract: %v", err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatalf("removing target file: %v", err)
	}
	// sidecar hash file is deliberately left behind — simulates a user
	// deleting the extracted binary but not the sidecar.

	if err := extractIfChanged(path, data, 0o755); err != nil {
		t.Fatalf("re-extract after deletion: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("expected file to be recreated, stat error: %v", err)
	}
}
