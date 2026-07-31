package hub

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/samosvalishe/free-turn-proxy/internal/provider"
)

const testPin = "MDEyMzQ1Njc4OWFiY2RlZjAxMjM0NTY3ODlhYmNkZWY=" // 32 байта base64

func TestNewRejectsBadConfig(t *testing.T) {
	tests := []struct {
		name string
		cfg  Config
		want string
	}{
		{"no URL", Config{Token: "t", PinSPKI: testPin}, "empty URL"},
		{"no token", Config{URL: "https://x", PinSPKI: testPin}, "empty token"},
		{"no pin", Config{URL: "https://x", Token: "t"}, "base64 SHA-256"},
		{"short pin", Config{URL: "https://x", Token: "t", PinSPKI: "YWJj"}, "base64 SHA-256"},
		{"not base64", Config{URL: "https://x", Token: "t", PinSPKI: "!!!"}, "base64 SHA-256"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := New(tt.cfg)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("New() error = %v, want containing %q", err, tt.want)
			}
		})
	}
}

func TestParseAddrs(t *testing.T) {
	tests := []struct {
		name string
		resp string
		want []string
	}{
		{"turn as string", `{"turn":"1.2.3.4:19302"}`, []string{"1.2.3.4:19302"}},
		{"turn strips scheme", `{"turn":"turn:1.2.3.4:19302"}`, []string{"1.2.3.4:19302"}},
		{"turns scheme", `{"turn":"turns:1.2.3.4:19302"}`, []string{"1.2.3.4:19302"}},
		{"strips query", `{"turn":"turn:1.2.3.4:19302?transport=tcp"}`, []string{"1.2.3.4:19302"}},
		{
			"turn as array keeps order",
			`{"turn":["turn:1.1.1.1:19302","turn:2.2.2.2:19302"]}`,
			[]string{"1.1.1.1:19302", "2.2.2.2:19302"},
		},
		{
			"turns field",
			`{"turns":["turn:1.1.1.1:19302","2.2.2.2:19302"]}`,
			[]string{"1.1.1.1:19302", "2.2.2.2:19302"},
		},
		{
			"dedup across fields",
			`{"turn":"turn:1.1.1.1:19302","turns":["1.1.1.1:19302","2.2.2.2:19302"]}`,
			[]string{"1.1.1.1:19302", "2.2.2.2:19302"},
		},
		{"empty", `{}`, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var hr hubResponse
			mustUnmarshal(t, tt.resp, &hr)
			got := parseAddrs(hr)
			if len(got) != len(tt.want) {
				t.Fatalf("parseAddrs() = %v, want %v", got, tt.want)
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Errorf("parseAddrs()[%d] = %q, want %q", i, got[i], tt.want[i])
				}
			}
		})
	}
}

func TestExpiryFromUsername(t *testing.T) {
	tests := []struct {
		user string
		want int64 // 0 = нулевое время
	}{
		{"1800000000:590238399207", 1800000000},
		{"590238399207", 0},         // нет двоеточия
		{"abc:12345", 0},            // не число
		{"-5:12345", 0},             // отрицательное
		{"", 0},                     // пусто
		{"1800000000:", 1800000000}, // userId пустой - допустимо
	}
	for _, tt := range tests {
		t.Run(tt.user, func(t *testing.T) {
			got := expiryFromUsername(tt.user)
			if tt.want == 0 {
				if !got.IsZero() {
					t.Errorf("expiryFromUsername(%q) = %v, want zero", tt.user, got)
				}
				return
			}
			if got.Unix() != tt.want {
				t.Errorf("expiryFromUsername(%q) = %d, want %d", tt.user, got.Unix(), tt.want)
			}
		})
	}
}

// newPinnedServer поднимает TLS-сервер и возвращает его URL плюс настоящий
// SPKI-пин его сертификата.
func newPinnedServer(t *testing.T, h http.HandlerFunc) (url, pin string) {
	t.Helper()
	ts := httptest.NewTLSServer(h)
	t.Cleanup(ts.Close)
	sum := sha256.Sum256(ts.Certificate().RawSubjectPublicKeyInfo)
	return ts.URL, base64.StdEncoding.EncodeToString(sum[:])
}

func TestGetCredentialsSuccess(t *testing.T) {
	var gotAuth string
	url, pin := newPinnedServer(t, func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		expiry := time.Now().Add(8 * time.Hour).Unix()
		_, _ = fmt.Fprintf(w, `{"username":"%d:590238399207","password":"pw",`+
			`"turn":["turn:1.1.1.1:19302","turn:2.2.2.2:19302"]}`, expiry)
	})

	p, err := New(Config{URL: url, PinSPKI: pin, Token: "secret"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	creds, err := p.GetCredentials(context.Background(), 0)
	if err != nil {
		t.Fatalf("GetCredentials: %v", err)
	}
	if gotAuth != "Bearer secret" {
		t.Errorf("Authorization = %q, want %q", gotAuth, "Bearer secret")
	}
	if creds.User == "" || creds.Pass != "pw" {
		t.Errorf("creds = %+v", creds)
	}
	// Оба кандидата должны дойти до pipeline: DialTURN перебирает их при
	// неудачном allocate.
	if len(creds.ServerAddrs) != 2 {
		t.Fatalf("ServerAddrs = %v, want 2 candidates", creds.ServerAddrs)
	}
	if creds.ServerAddrs[0] != "1.1.1.1:19302" {
		t.Errorf("primary addr = %q, want 1.1.1.1:19302", creds.ServerAddrs[0])
	}
	if creds.ExpiresAt.IsZero() {
		t.Error("ExpiresAt should be parsed from username")
	}
}

func TestGetCredentialsCaches(t *testing.T) {
	var hits int
	url, pin := newPinnedServer(t, func(w http.ResponseWriter, _ *http.Request) {
		hits++
		expiry := time.Now().Add(8 * time.Hour).Unix()
		_, _ = fmt.Fprintf(w, `{"username":"%d:1","password":"pw","turn":"1.1.1.1:19302"}`, expiry)
	})
	p, err := New(Config{URL: url, PinSPKI: pin, Token: "t"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	for i := range 5 {
		if _, err := p.GetCredentials(context.Background(), i); err != nil {
			t.Fatalf("GetCredentials(%d): %v", i, err)
		}
	}
	if hits != 1 {
		t.Errorf("hub hits = %d, want 1 (cache shared across streams)", hits)
	}
}

// Pipeline поднимает N стримов одновременно, и все они зовут GetCredentials
// на холодном кеше. В хаб при этом должен уйти ОДИН запрос, а не N.
func TestGetCredentialsConcurrentColdStartFetchesOnce(t *testing.T) {
	var hits atomic.Int32
	url, pin := newPinnedServer(t, func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		// Задержка расширяет окно гонки: без сериализации все горутины
		// успеют промахнуться мимо кеша до того, как первая его заполнит.
		time.Sleep(50 * time.Millisecond)
		expiry := time.Now().Add(8 * time.Hour).Unix()
		_, _ = fmt.Fprintf(w, `{"username":"%d:1","password":"pw","turn":"1.1.1.1:19302"}`, expiry)
	})
	p, err := New(Config{URL: url, PinSPKI: pin, Token: "t"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	const streams = 10
	var wg sync.WaitGroup
	errs := make(chan error, streams)
	for i := range streams {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := p.GetCredentials(context.Background(), i); err != nil {
				errs <- err
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("GetCredentials: %v", err)
	}

	if got := hits.Load(); got != 1 {
		t.Errorf("hub hits = %d, want 1 (%d streams must not stampede the hub)", got, streams)
	}
}

func TestGetCredentialsPinMismatch(t *testing.T) {
	url, _ := newPinnedServer(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprint(w, `{"username":"1:1","password":"pw","turn":"1.1.1.1:19302"}`)
	})
	// Валидный по форме, но чужой пин.
	p, err := New(Config{URL: url, PinSPKI: testPin, Token: "t"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	_, err = p.GetCredentials(context.Background(), 0)
	if err == nil {
		t.Fatal("expected pin mismatch error, got nil")
	}
	if !strings.Contains(err.Error(), "pin mismatch") {
		t.Errorf("error = %v, want pin mismatch", err)
	}
}

func TestGetCredentialsHTTPErrorEntersBackoff(t *testing.T) {
	url, pin := newPinnedServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	})
	p, err := New(Config{URL: url, PinSPKI: pin, Token: "bad"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	_, err = p.GetCredentials(context.Background(), 0)
	if !errors.Is(err, provider.ErrBackoffActive) {
		t.Fatalf("error = %v, want ErrBackoffActive", err)
	}
	if p.BackoffUntilUnix() == 0 {
		t.Error("BackoffUntilUnix() = 0, want a deadline after failure")
	}
	// Второй заход должен вернуться из бэк-оффа, не ходя в сеть.
	if _, err := p.GetCredentials(context.Background(), 0); !errors.Is(err, provider.ErrBackoffActive) {
		t.Errorf("second call error = %v, want ErrBackoffActive", err)
	}
}

func TestGetCredentialsRejectsResponseWithoutAddr(t *testing.T) {
	url, pin := newPinnedServer(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprint(w, `{"username":"1:1","password":"pw"}`)
	})
	p, err := New(Config{URL: url, PinSPKI: pin, Token: "t"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := p.GetCredentials(context.Background(), 0); err == nil {
		t.Fatal("expected error for response without TURN address")
	}
}

func TestHandleAuthErrorInvalidatesAtThreshold(t *testing.T) {
	var hits int
	url, pin := newPinnedServer(t, func(w http.ResponseWriter, _ *http.Request) {
		hits++
		expiry := time.Now().Add(8 * time.Hour).Unix()
		_, _ = fmt.Fprintf(w, `{"username":"%d:1","password":"pw","turn":"1.1.1.1:19302"}`, expiry)
	})
	p, err := New(Config{URL: url, PinSPKI: pin, Token: "t"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := p.GetCredentials(context.Background(), 0); err != nil {
		t.Fatalf("GetCredentials: %v", err)
	}

	for i := 1; i < authErrorThreshold; i++ {
		if p.HandleAuthError(0) {
			t.Fatalf("HandleAuthError invalidated early at %d", i)
		}
	}
	if !p.HandleAuthError(0) {
		t.Fatal("HandleAuthError should invalidate at threshold")
	}
	// Кеш сброшен -> следующий вызов идёт в хаб.
	if _, err := p.GetCredentials(context.Background(), 0); err != nil {
		t.Fatalf("GetCredentials after invalidation: %v", err)
	}
	if hits != 2 {
		t.Errorf("hub hits = %d, want 2 (refetch after invalidation)", hits)
	}
}

func TestResetErrorsClearsCounters(t *testing.T) {
	p := &Provider{authErrors: map[int]int{}, invalidations: 3}
	p.authErrors[7] = authErrorThreshold - 1
	p.ResetErrors(7)
	if p.invalidations != 0 {
		t.Errorf("invalidations = %d, want 0", p.invalidations)
	}
	// Счётчик стрима сброшен -> порог снова полный.
	if p.HandleAuthError(7) {
		t.Error("HandleAuthError invalidated immediately after ResetErrors")
	}
}

func TestIsAuthError(t *testing.T) {
	p := &Provider{}
	for msg, want := range map[string]bool{
		"allocate: 401 Unauthorized": true,
		"stale nonce":                true,
		"invalid credential":         true,
		"authentication failed":      true,
		"STALE NONCE":                true, // регистр не гарантирован
		"Invalid Credential":         true,
		"connection refused":         false,
		"i/o timeout":                false,
		// 403 - неразрешённый peer-адрес, а не протухшие креды.
		"allocate: 403 Forbidden": false,
	} {
		if got := p.IsAuthError(errors.New(msg)); got != want {
			t.Errorf("IsAuthError(%q) = %v, want %v", msg, got, want)
		}
	}
	if p.IsAuthError(nil) {
		t.Error("IsAuthError(nil) = true, want false")
	}
}

func TestName(t *testing.T) {
	if (&Provider{}).Name() != "hub" {
		t.Errorf("Name() = %q, want hub", (&Provider{}).Name())
	}
}

func TestCacheDeadlineFallsBackWhenExpiryUnknown(t *testing.T) {
	got := cacheDeadline(time.Time{})
	if got.Before(time.Now().Add(fallbackLifetime - time.Minute)) {
		t.Errorf("cacheDeadline(zero) = %v, want ~%v ahead", got, fallbackLifetime)
	}
	// Почти протухшие креды не должны кешироваться на полный срок.
	soon := cacheDeadline(time.Now().Add(time.Second))
	if soon.After(time.Now().Add(2 * time.Minute)) {
		t.Errorf("cacheDeadline(near-expiry) = %v, want short", soon)
	}
}

func mustUnmarshal(t *testing.T, s string, v any) {
	t.Helper()
	if err := json.Unmarshal([]byte(s), v); err != nil {
		t.Fatalf("unmarshal %s: %v", s, err)
	}
}
