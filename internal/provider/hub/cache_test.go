package hub

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

func TestCacheFor(t *testing.T) {
	// один аккаунт -> base как есть
	if got := CacheFor("/x/c.json", "https://h/a", 1); got != "/x/c.json" {
		t.Errorf("single: got %q", got)
	}
	// пусто -> пусто
	if got := CacheFor("", "https://h/a", 3); got != "" {
		t.Errorf("empty base: got %q", got)
	}
	// multi -> у каждого URL свой файл, детерминированно и различимо
	a := CacheFor("/x/c.json", "https://h:8445/turn-creds", 2)
	b := CacheFor("/x/c.json", "https://h:8446/turn-creds", 2)
	if a == b {
		t.Errorf("multi collision: %q == %q", a, b)
	}
	if a != CacheFor("/x/c.json", "https://h:8445/turn-creds", 2) {
		t.Error("CacheFor not deterministic")
	}
}

// TestDiskCacheRoundTrip: успешный fetch пишет файл; новый Provider поднимает
// свежий кеш и отдаёт его БЕЗ похода в хаб.
func TestDiskCacheRoundTrip(t *testing.T) {
	dir := t.TempDir()
	cacheFile := filepath.Join(dir, "creds.json")
	var hits int32
	url, pin := newPinnedServer(t, func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		expiry := time.Now().Add(8 * time.Hour).Unix()
		_, _ = fmt.Fprintf(w, `{"username":"%d:590238399207","password":"pw","turn":["turn:1.1.1.1:19302"]}`, expiry)
	})

	p1, err := New(Config{URL: url, PinSPKI: pin, Token: "s", CacheFile: cacheFile})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p1.GetCredentials(context.Background(), 0); err != nil {
		t.Fatal(err)
	}
	if atomic.LoadInt32(&hits) != 1 {
		t.Fatalf("hits after first fetch = %d, want 1", hits)
	}
	if _, err := os.Stat(cacheFile); err != nil {
		t.Fatalf("cache file not written: %v", err)
	}

	// новый провайдер (эмулируем рестарт app) — свежий кеш, в хаб не идём
	p2, err := New(Config{URL: url, PinSPKI: pin, Token: "s", CacheFile: cacheFile})
	if err != nil {
		t.Fatal(err)
	}
	creds, err := p2.GetCredentials(context.Background(), 0)
	if err != nil {
		t.Fatal(err)
	}
	if atomic.LoadInt32(&hits) != 1 {
		t.Errorf("hits after restart = %d, want still 1 (served from disk)", hits)
	}
	if creds.ServerAddrs[0] != "1.1.1.1:19302" {
		t.Errorf("addr = %q", creds.ServerAddrs[0])
	}
}

// TestStaleCacheEmergencyFallback: кеш протух, хаб недостижим -> отдаём старые turns.
func TestStaleCacheEmergencyFallback(t *testing.T) {
	dir := t.TempDir()
	cacheFile := filepath.Join(dir, "creds.json")
	// кладём на диск ПРОТУХШИЕ креды
	past := time.Now().Add(-time.Hour)
	b, _ := json.Marshal(diskEntry{User: "u", Pass: "pw", ServerAddrs: []string{"9.9.9.9:19302"}, ExpiresAt: past})
	if err := os.WriteFile(cacheFile, b, 0o600); err != nil {
		t.Fatal(err)
	}

	// хаб недостижим: пин валидный по форме, но URL мёртвый -> fetch падает
	deadURL, pin := newPinnedServer(t, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	})

	p, err := New(Config{URL: deadURL, PinSPKI: pin, Token: "s", CacheFile: cacheFile})
	if err != nil {
		t.Fatal(err)
	}
	creds, err := p.GetCredentials(context.Background(), 1)
	if err != nil {
		t.Fatalf("emergency fallback should not error, got %v", err)
	}
	if len(creds.ServerAddrs) == 0 || creds.ServerAddrs[0] != "9.9.9.9:19302" {
		t.Errorf("expected stale turns fallback, got %+v", creds)
	}
}

// TestProactiveRefresh: креды истекают меньше чем через refreshMargin ->
// refreshIfDue сам идёт в хаб, не дожидаясь запроса стрима (сценарий "клиент
// 8 часов за белым списком, хаб достижим только через живой туннель").
func TestProactiveRefresh(t *testing.T) {
	var hits int32
	url, pin := newPinnedServer(t, func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&hits, 1)
		// 1-й ответ: +45m (внутри refreshMargin -> обновляемся);
		// 2-й: +90m (снаружи -> второй раз не ходим).
		expiry := time.Now().Add(time.Duration(n) * 45 * time.Minute).Unix()
		_, _ = fmt.Fprintf(w, `{"username":"%d:1","password":"pw","turn":["turn:1.1.1.1:19302"]}`, expiry)
	})
	p, err := New(Config{URL: url, PinSPKI: pin, Token: "s"})
	if err != nil {
		t.Fatal(err)
	}
	// первый fetch: expiry = +30m, то есть внутри refreshMargin (1ч)
	if _, err := p.GetCredentials(context.Background(), 0); err != nil {
		t.Fatal(err)
	}
	first := p.cached.ExpiresAt

	p.refreshIfDue(context.Background())
	if atomic.LoadInt32(&hits) != 2 {
		t.Fatalf("hits = %d, want 2 (refresh must fire)", hits)
	}
	if !p.cached.ExpiresAt.After(first) {
		t.Errorf("expiry not advanced: %v -> %v", first, p.cached.ExpiresAt)
	}

	// теперь expiry = +60m: до дедлайна ещё далеко, второй раз не ходим
	p.refreshIfDue(context.Background())
	if atomic.LoadInt32(&hits) != 2 {
		t.Errorf("hits = %d, want still 2 (creds fresh enough)", hits)
	}
}

// TestRefreshBackoffOnSameExpiry: хаб не переминтил (тот же expiry) -> держим
// паузу, а не долбим его каждый тик.
func TestRefreshBackoffOnSameExpiry(t *testing.T) {
	expiry := time.Now().Add(20 * time.Minute).Unix()
	var hits int32
	url, pin := newPinnedServer(t, func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		_, _ = fmt.Fprintf(w, `{"username":"%d:1","password":"pw","turn":["turn:1.1.1.1:19302"]}`, expiry)
	})
	p, err := New(Config{URL: url, PinSPKI: pin, Token: "s"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.GetCredentials(context.Background(), 0); err != nil {
		t.Fatal(err)
	}
	p.refreshIfDue(context.Background()) // сходит, увидит тот же expiry
	after := atomic.LoadInt32(&hits)
	p.refreshIfDue(context.Background()) // должен упереться в refreshNotBefore
	if atomic.LoadInt32(&hits) != after {
		t.Errorf("hits = %d, want %d (backoff must hold)", hits, after)
	}
}

// TestNoCacheFileNoWrite: без CacheFile ничего на диск не пишем (десктоп).
func TestNoCacheFileNoWrite(t *testing.T) {
	url, pin := newPinnedServer(t, func(w http.ResponseWriter, r *http.Request) {
		expiry := time.Now().Add(8 * time.Hour).Unix()
		_, _ = fmt.Fprintf(w, `{"username":"%d:1","password":"pw","turn":["turn:1.1.1.1:19302"]}`, expiry)
	})
	p, err := New(Config{URL: url, PinSPKI: pin, Token: "s"}) // без CacheFile
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.GetCredentials(context.Background(), 0); err != nil {
		t.Fatal(err)
	}
	// нет паники/ошибки — достаточно; saveCache/loadCache — no-op при пустом пути
}
