// Package hub - провайдер TURN-реквизитов, забирающий уже добытые креды с
// доверенного HTTP-эндпоинта ("хаба") вместо похода в VK API.
//
// Мотивация: выдача anonymToken в VK защищена captcha, и каждый клиент,
// ходящий в API сам, упирается в неё независимо. Хаб минтит креды один раз
// централизованно и раздаёт готовые - клиенты VK API не трогают вовсе.
//
// Транспорт: HTTPS с самоподписанным сертификатом, доверие через SPKI-пин
// (сверка SHA-256 отпечатка публичного ключа), авторизация Bearer-токеном.
// Обычная цепочка CA намеренно не используется - у хаба нет публичного имени.
//
// Ответ эндпоинта:
//
//	{"username":"<unix-expiry>:<userId>","password":"...","turn":"host:port"}
//
// Поле turn принимается и как строка (историческая форма), и как массив
// строк ("turns"): pipeline умеет перебирать кандидатов при неудачном
// allocate, поэтому список сохраняется целиком.
package hub

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/samosvalishe/free-turn-proxy/internal/logx"
	"github.com/samosvalishe/free-turn-proxy/internal/provider"
)

const (
	// backoffAfterFailure - пауза перед повторным походом в хаб после ошибки.
	// Короткая: хаб наш, недоступность обычно означает рестарт юнита.
	backoffAfterFailure = 30 * time.Second

	// cacheSafetyMargin - за сколько до истечения считать креды протухшими.
	cacheSafetyMargin = 5 * time.Minute

	// fallbackLifetime - на сколько кешировать, если expiry из username не
	// разобрался. Консервативно мало: лучше лишний запрос, чем мёртвые креды.
	fallbackLifetime = 10 * time.Minute

	// authErrorThreshold - сколько auth-ошибок подряд на стрим терпеть до
	// инвалидации кеша.
	authErrorThreshold = 3

	// maxAuthInvalidations - сколько раз подряд инвалидировать кеш, не увидев
	// ни одного успешного allocate, прежде чем признать ситуацию фатальной.
	//
	// Хаб не умеет переминчивать креды сам: если anonymToken на хабе умер,
	// hubcreds продолжает отдавать last-good, TURN отвечает 401, и без этого
	// счётчика клиент крутит бесконечный цикл "инвалидация -> тот же мёртвый
	// кред -> 401". Порог превращает цикл во внятный фатал.
	maxAuthInvalidations = 5

	httpTimeout = 15 * time.Second
)

// Config - параметры hub-провайдера.
type Config struct {
	// URL эндпоинта, отдающего креды. Обязателен.
	URL string

	// PinSPKI - base64 SHA-256 отпечатка SubjectPublicKeyInfo сертификата
	// хаба. Обязателен: без него самоподписанный серт нечем проверить.
	PinSPKI string

	// Token - Bearer-токен авторизации. Обязателен.
	Token string

	// Dialer используется для исходящего соединения к хабу.
	Dialer net.Dialer

	// Log - уровневый логгер. nil -> no-op.
	Log logx.Logger
}

// Provider реализует provider.Provider поверх хаба. Кеш общий на все стримы:
// креды VK валидны для всего TURN-пула, разделять их по streamID незачем.
type Provider struct {
	cfg  Config
	log  logx.Logger
	http *http.Client

	mu            sync.Mutex
	cached        provider.Credentials
	cachedUntil   time.Time
	backoffUntil  time.Time
	authErrors    map[int]int
	invalidations int
}

func New(cfg Config) (*Provider, error) {
	if cfg.URL == "" {
		return nil, errors.New("hub: empty URL")
	}
	if cfg.Token == "" {
		return nil, errors.New("hub: empty token (set VKTURN_HUB_TOKEN)")
	}
	pin, err := base64.StdEncoding.DecodeString(cfg.PinSPKI)
	if err != nil || len(pin) != sha256.Size {
		return nil, fmt.Errorf("hub: -hub-pin must be base64 SHA-256 of SPKI (%d bytes)", sha256.Size)
	}
	return &Provider{
		cfg:        cfg,
		log:        logx.OrNop(cfg.Log),
		http:       newPinnedClient(cfg.Dialer, pin),
		authErrors: make(map[int]int),
	}, nil
}

// newPinnedClient строит HTTP-клиент, доверяющий ровно одному публичному
// ключу. Проверка стандартной цепочки отключена намеренно (серт
// самоподписанный, у хаба нет публичного имени), её заменяет сверка SPKI.
func newPinnedClient(dialer net.Dialer, pin []byte) *http.Client {
	return &http.Client{
		Timeout: httpTimeout,
		Transport: &http.Transport{
			DialContext: dialer.DialContext,
			TLSClientConfig: &tls.Config{
				// Цепочка не проверяется - доверие целиком на VerifyConnection.
				InsecureSkipVerify: true, //nolint:gosec // заменено пином SPKI ниже
				MinVersion:         tls.VersionTLS12,
				VerifyConnection: func(cs tls.ConnectionState) error {
					if len(cs.PeerCertificates) == 0 {
						return errors.New("hub: no peer certificate")
					}
					return verifyPin(cs.PeerCertificates[0], pin)
				},
			},
		},
	}
}

func verifyPin(cert *x509.Certificate, pin []byte) error {
	sum := sha256.Sum256(cert.RawSubjectPublicKeyInfo)
	if subtleEqual(sum[:], pin) {
		return nil
	}
	return fmt.Errorf("hub: SPKI pin mismatch (got %s)", base64.StdEncoding.EncodeToString(sum[:]))
}

// subtleEqual - постоянное по времени сравнение. Пин не секрет, но
// расхождений в стоимости сравнения тут быть не должно.
func subtleEqual(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	var diff byte
	for i := range a {
		diff |= a[i] ^ b[i]
	}
	return diff == 0
}

// hubResponse - форма ответа hubcreds. turn/turns взаимозаменяемы.
type hubResponse struct {
	Username string          `json:"username"`
	Password string          `json:"password"`
	Turn     json.RawMessage `json:"turn"`
	Turns    []string        `json:"turns"`
}

func (p *Provider) GetCredentials(ctx context.Context, streamID int) (provider.Credentials, error) {
	p.mu.Lock()
	if time.Now().Before(p.cachedUntil) && len(p.cached.ServerAddrs) > 0 {
		c := p.cached
		p.mu.Unlock()
		return c, nil
	}
	if until := p.backoffUntil; time.Now().Before(until) {
		p.mu.Unlock()
		return provider.Credentials{}, fmt.Errorf("%w: hub unreachable, retry after %s",
			provider.ErrBackoffActive, until.Format(time.TimeOnly))
	}
	p.mu.Unlock()

	creds, err := p.fetch(ctx, streamID)
	if err != nil {
		p.mu.Lock()
		p.backoffUntil = time.Now().Add(backoffAfterFailure)
		p.mu.Unlock()
		return provider.Credentials{}, fmt.Errorf("%w: %w", provider.ErrBackoffActive, err)
	}

	p.mu.Lock()
	p.cached = creds
	p.cachedUntil = cacheDeadline(creds.ExpiresAt)
	p.backoffUntil = time.Time{}
	p.mu.Unlock()

	p.log.Infof("[STREAM %d] [Hub] creds fetched, turn=%s (+%d candidates), expires %s",
		streamID, creds.ServerAddrs[0], len(creds.ServerAddrs)-1, creds.ExpiresAt.Format(time.RFC3339))
	return creds, nil
}

// cacheDeadline - до какого момента держать креды в кеше.
func cacheDeadline(expiresAt time.Time) time.Time {
	if expiresAt.IsZero() {
		return time.Now().Add(fallbackLifetime)
	}
	deadline := expiresAt.Add(-cacheSafetyMargin)
	if !deadline.After(time.Now()) {
		// Креды уже почти протухли - взять их, но не кешировать надолго.
		return time.Now().Add(time.Minute)
	}
	return deadline
}

func (p *Provider) fetch(ctx context.Context, streamID int) (provider.Credentials, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.cfg.URL, nil)
	if err != nil {
		return provider.Credentials{}, fmt.Errorf("hub: build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+p.cfg.Token)

	resp, err := p.http.Do(req)
	if err != nil {
		return provider.Credentials{}, fmt.Errorf("hub: request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if err != nil {
		return provider.Credentials{}, fmt.Errorf("hub: read body: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return provider.Credentials{}, fmt.Errorf("hub: HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	var hr hubResponse
	if err := json.Unmarshal(body, &hr); err != nil {
		return provider.Credentials{}, fmt.Errorf("hub: decode: %w", err)
	}
	if hr.Username == "" || hr.Password == "" {
		return provider.Credentials{}, errors.New("hub: response missing username/password")
	}

	addrs := parseAddrs(hr)
	if len(addrs) == 0 {
		return provider.Credentials{}, errors.New("hub: response has no TURN address")
	}

	return provider.Credentials{
		User:        hr.Username,
		Pass:        hr.Password,
		ServerAddrs: addrs,
		ExpiresAt:   expiryFromUsername(hr.Username),
	}, nil
}

// parseAddrs собирает кандидатов host:port из полей turn/turns, снимая схему
// turn:/turns: и хвост ?transport. Порядок ответа сохраняется - hubcreds
// отдаёт его primary-first, как того ждёт provider.Credentials.
func parseAddrs(hr hubResponse) []string {
	var raw []string

	// turn: строка (историческая форма) либо массив.
	if len(hr.Turn) > 0 {
		var one string
		if err := json.Unmarshal(hr.Turn, &one); err == nil {
			raw = append(raw, one)
		} else {
			var many []string
			if err := json.Unmarshal(hr.Turn, &many); err == nil {
				raw = append(raw, many...)
			}
		}
	}
	raw = append(raw, hr.Turns...)

	seen := make(map[string]bool, len(raw))
	addrs := make([]string, 0, len(raw))
	for _, r := range raw {
		a := normalizeAddr(r)
		if a == "" || seen[a] {
			continue
		}
		seen[a] = true
		addrs = append(addrs, a)
	}
	return addrs
}

func normalizeAddr(s string) string {
	s = strings.TrimSpace(s)
	s = strings.SplitN(s, "?", 2)[0]
	s = strings.TrimPrefix(strings.TrimPrefix(s, "turns:"), "turn:")
	return strings.TrimSpace(s)
}

// expiryFromUsername достаёт дедлайн из TURN-username вида "<unix>:<userId>".
// Нулевое время - формат не распознан, вызывающий подставит fallback.
func expiryFromUsername(user string) time.Time {
	head, _, ok := strings.Cut(user, ":")
	if !ok {
		return time.Time{}
	}
	unix, err := strconv.ParseInt(head, 10, 64)
	if err != nil || unix <= 0 {
		return time.Time{}
	}
	return time.Unix(unix, 0)
}

// IsAuthError - ошибки TURN-allocate, означающие протухшие креды. Набор тот
// же, что у VK-провайдера: pipeline трактует их одинаково независимо от
// источника кредов.
func (*Provider) IsAuthError(err error) bool {
	if err == nil {
		return false
	}
	s := err.Error()
	return strings.Contains(s, "401") ||
		strings.Contains(s, "Unauthorized") ||
		strings.Contains(s, "authentication") ||
		strings.Contains(s, "invalid credential") ||
		strings.Contains(s, "stale nonce")
}

// HandleAuthError считает auth-ошибки и по достижении порога сбрасывает кеш,
// чтобы следующий GetCredentials сходил в хаб за свежими кредами.
func (p *Provider) HandleAuthError(streamID int) bool {
	p.mu.Lock()
	defer p.mu.Unlock()

	p.authErrors[streamID]++
	if p.authErrors[streamID] < authErrorThreshold {
		return false
	}
	p.authErrors[streamID] = 0

	p.cached = provider.Credentials{}
	p.cachedUntil = time.Time{}
	p.invalidations++

	if p.invalidations >= maxAuthInvalidations {
		// Хаб раз за разом отдаёт креды, которые TURN не принимает: на хабе
		// умер anonymToken и hubcreds раздаёт last-good. Сам себя провайдер
		// вылечить не может - нужен человек.
		p.log.Errorf("[Hub] %d cache invalidations without a successful allocate: "+
			"hub is serving credentials TURN rejects (dead anonymToken on the hub?)", p.invalidations)
	}
	return true
}

// ResetErrors вызывается после успешного allocate: снимает и счётчик ошибок
// стрима, и общий счётчик инвалидаций.
func (p *Provider) ResetErrors(streamID int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.authErrors, streamID)
	p.invalidations = 0
}

func (p *Provider) BackoffUntilUnix() int64 {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.backoffUntil.IsZero() {
		return 0
	}
	return p.backoffUntil.Unix()
}

func (*Provider) Name() string { return "hub" }
