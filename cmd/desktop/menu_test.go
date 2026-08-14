// cmd/desktop/menu_test.go
package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestSelectFromKeysArrowsAndEnter(t *testing.T) {
	items := []string{"vk-turn", "xray-подписка", "выход"}
	// down, down, up, enter -> index 1
	keys := strings.NewReader("\x1b[B\x1b[B\x1b[A\r")
	var out bytes.Buffer
	idx, err := selectFromKeys(items, keys, &out)
	if err != nil {
		t.Fatalf("selectFromKeys: %v", err)
	}
	if idx != 1 {
		t.Fatalf("expected index 1, got %d", idx)
	}
	if !strings.Contains(out.String(), "xray-подписка") {
		t.Fatalf("expected rendered output to mention selection, got %q", out.String())
	}
}

func TestSelectFromKeysWrapsAtBoundaries(t *testing.T) {
	items := []string{"a", "b"}
	// up from index 0 wraps to last item, then enter
	keys := strings.NewReader("\x1b[A\r")
	idx, err := selectFromKeys(items, keys, &bytes.Buffer{})
	if err != nil {
		t.Fatal(err)
	}
	if idx != 1 {
		t.Fatalf("expected wrap to index 1, got %d", idx)
	}
}

func TestSelectFromKeysCtrlCReturnsError(t *testing.T) {
	items := []string{"a", "b"}
	keys := strings.NewReader("\x03")
	_, err := selectFromKeys(items, keys, &bytes.Buffer{})
	if err == nil {
		t.Fatal("expected error on Ctrl+C")
	}
}
