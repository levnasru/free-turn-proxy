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
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
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

	// refreshMargin - за сколько до истечения фоново сходить за свежими кредами.
	//
	// Ленивого обновления не хватает: поднятые стримы за кредами больше не
	// ходят, поэтому клиент, просидевший за операторским белым списком дольше
	// срока жизни кредов (~8ч), теряет и RAM-кеш, и дисковый - а хаб к тому
	// моменту достижим только ЧЕРЕЗ туннель, который без кредов не поднять.
	// Обновляемся заранее, пока туннель ещё жив.
	refreshMargin = time.Hour

	// refreshTick - как часто проверять, не пора ли обновляться.
	refreshTick = time.Minute

	// refreshRetryAfter - пауза после безрезультатной попытки фонового
	// обновления (хаб не ответил или отдал те же креды с тем же expiry).
	// Отдельно от backoffUntil: тот блокирует и обычный GetCredentials, а
	// текущие креды ещё живы и новым стримам их отдавать можно.
	refreshRetryAfter = 5 * time.Minute

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

	// CacheFile - путь к дисковому кешу кредов (опционально). Нужен там, где
	// хаб недостижим напрямую (Android за операторским белым списком): холодный
	// старт/реконнект поднимает туннель на кешированных кредах, не стуча в хаб,
	// а свежие подтягиваются уже ЧЕРЕЗ туннель и перезаписывают кеш. Пусто -
	// кеш только в RAM (десктоп, где хаб доступен напрямую).
	CacheFile string

	// Ctx - контекст жизни сессии. Задан -> провайдер поднимает фоновое
	// обновление кредов (см. refreshMargin) и глушит его по отмене. nil ->
	// обновление только ленивое, по запросу стрима.
	Ctx context.Context
}

// Provider реализует provider.Provider поверх хаба. Кеш общий на все стримы:
// креды VK валидны для всего TURN-пула, разделять их по streamID незачем.
type Provider struct {
	cfg  Config
	log  logx.Logger
	http *http.Client

	// mu держится и на время похода в хаб: pipeline поднимает N стримов
	// разом, и без сериализации все они промахиваются мимо холодного кеша
	// одновременно, устраивая хабу N-кратный залп. Первый заполняет кеш,
	// остальные получают готовое.
	mu            sync.Mutex
	cached        provider.Credentials
	cachedUntil   time.Time
	authErrors    map[int]int
	invalidations int

	// refreshNotBefore - до какого момента не повторять фоновое обновление
	// после безрезультатной попытки (см. refreshRetryAfter).
	refreshNotBefore time.Time

	// backoffUntil - unix-секунды, отдельно от mu: BackoffUntilUnix зовут
	// из pipeline, и он не должен ждать чужой fetch.
	backoffUntil atomic.Int64
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
	p := &Provider{
		cfg:        cfg,
		log:        logx.OrNop(cfg.Log),
		http:       newPinnedClient(cfg.Dialer, pin),
		authErrors: make(map[int]int),
	}
	p.loadCache()
	if cfg.Ctx != nil {
		go p.refreshLoop(cfg.Ctx)
	}
	return p, nil
}

// refreshLoop заранее перезабирает креды, пока туннель ещё жив (см.
// refreshMargin). Без него клиент, просидевший за белым списком дольше срока
// жизни кредов, остаётся и без кеша, и без доступа к хабу.
func (p *Provider) refreshLoop(ctx context.Context) {
	t := time.NewTicker(refreshTick)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			p.refreshIfDue(ctx)
		}
	}
}

func (p *Provider) refreshIfDue(ctx context.Context) {
	p.mu.Lock()
	defer p.mu.Unlock()

	now := time.Now()
	// Кредов ещё не было, либо expiry неизвестен - обновит ленивый путь.
	if len(p.cached.ServerAddrs) == 0 || p.cached.ExpiresAt.IsZero() {
		return
	}
	if now.Before(p.refreshNotBefore) {
		return
	}
	if now.Before(p.cached.ExpiresAt.Add(-refreshMargin)) {
		return
	}

	creds, err := p.fetch(ctx)
	if err != nil {
		p.refreshNotBefore = now.Add(refreshRetryAfter)
		p.log.Warnf("[Hub] proactive refresh failed (%v); current creds expire %s",
			err, p.cached.ExpiresAt.Format(time.RFC3339))
		return
	}
	if !creds.ExpiresAt.After(p.cached.ExpiresAt) {
		// Хаб ещё не переминтил - ждём, не долбим его каждую минуту.
		p.refreshNotBefore = now.Add(refreshRetryAfter)
		return
	}

	p.cached = creds
	p.cachedUntil = cacheDeadline(creds.ExpiresAt)
	p.refreshNotBefore = time.Time{}
	p.backoffUntil.Store(0)
	p.saveCache(creds)
	p.log.Infof("[Hub] creds refreshed proactively, turn=%s, expires %s",
		creds.ServerAddrs[0], creds.ExpiresAt.Format(time.RFC3339))
}

// CacheFor выводит путь кеша для конкретного hub-URL. При одном аккаунте отдаёт
// base как есть; при нескольких (multi-hub) даёт каждому свой файл (суффикс -
// короткий хеш URL), иначе провайдеры затирали бы креды друг друга. Пустой base
// -> пусто (кеш выключен).
func CacheFor(base, url string, total int) string {
	if base == "" || total <= 1 {
		return base
	}
	sum := sha256.Sum256([]byte(url))
	return base + "." + hex.EncodeToString(sum[:4])
}

// diskEntry - формат дискового кеша (см. Config.CacheFile).
type diskEntry struct {
	User        string    `json:"user"`
	Pass        string    `json:"pass"`
	ServerAddrs []string  `json:"server_addrs"`
	ExpiresAt   time.Time `json:"expires_at"`
}

// loadCache подгружает креды с диска в RAM-кеш, если файл есть и они не протухли.
// Позволяет холодному старту за белым списком оператора поднять туннель, не ходя
// в недостижимый хаб. Ошибки не фатальны - просто пойдём в хаб.
func (p *Provider) loadCache() {
	if p.cfg.CacheFile == "" {
		return
	}
	b, err := os.ReadFile(p.cfg.CacheFile)
	if err != nil {
		return
	}
	var e diskEntry
	if json.Unmarshal(b, &e) != nil || len(e.ServerAddrs) == 0 {
		return
	}
	creds := provider.Credentials{User: e.User, Pass: e.Pass, ServerAddrs: e.ServerAddrs, ExpiresAt: e.ExpiresAt}
	// cached держим всегда (даже протухшее) - это ЧС-фолбэк в fetchOrStale.
	// cachedUntil в будущее ставим только для реально свежих: иначе первый
	// GetCredentials сходит в хаб за свежими, а к старым откатится лишь при сбое.
	p.cached = creds
	deadline := cacheDeadline(creds.ExpiresAt)
	if deadline.After(time.Now()) {
		p.cachedUntil = deadline
		p.log.Infof("[Hub] creds loaded from cache (fresh), turn=%s, expires %s",
			creds.ServerAddrs[0], creds.ExpiresAt.Format(time.RFC3339))
	} else {
		p.log.Infof("[Hub] creds loaded from cache (stale, emergency fallback only), turn=%s", creds.ServerAddrs[0])
	}
}

// saveCache атомарно (temp+rename) пишет свежие креды на диск. Вызывается под p.mu.
func (p *Provider) saveCache(creds provider.Credentials) {
	if p.cfg.CacheFile == "" {
		return
	}
	b, err := json.Marshal(diskEntry{User: creds.User, Pass: creds.Pass, ServerAddrs: creds.ServerAddrs, ExpiresAt: creds.ExpiresAt})
	if err != nil {
		return
	}
	tmp := p.cfg.CacheFile + ".tmp"
	if os.WriteFile(tmp, b, 0o600) != nil {
		return
	}
	if err := os.Rename(tmp, p.cfg.CacheFile); err != nil {
		_ = os.Remove(tmp)
	}
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
	defer p.mu.Unlock()

	// Кеш мог заполниться, пока эта горутина ждала mu.
	if time.Now().Before(p.cachedUntil) && len(p.cached.ServerAddrs) > 0 {
		return p.cached, nil
	}
	if until := p.backoffUntil.Load(); until > 0 && time.Now().Unix() < until {
		return provider.Credentials{}, fmt.Errorf("%w: hub unreachable, retry after %s",
			provider.ErrBackoffActive, time.Unix(until, 0).Format(time.TimeOnly))
	}

	creds, err := p.fetch(ctx)
	if err != nil {
		p.backoffUntil.Store(time.Now().Add(backoffAfterFailure).Unix())
		// ЧС-фолбэк: хаб недостижим (белый список оператора), но на руках есть
		// прошлые turns. Часто VK их не меняет и старые креды ещё принимаются -
		// пробуем их, чтобы хоть как-то подняться. Не приняли -> обычный auth-путь.
		if len(p.cached.ServerAddrs) > 0 {
			p.log.Warnf("[STREAM %d] [Hub] fetch failed (%v); using stale cached creds as emergency fallback, turn=%s",
				streamID, err, p.cached.ServerAddrs[0])
			return p.cached, nil
		}
		return provider.Credentials{}, fmt.Errorf("%w: %w", provider.ErrBackoffActive, err)
	}

	p.cached = creds
	p.cachedUntil = cacheDeadline(creds.ExpiresAt)
	p.backoffUntil.Store(0)
	p.saveCache(creds)

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

// fetch ходит в хаб за свежими кредами. Вызывается под p.mu.
func (p *Provider) fetch(ctx context.Context) (provider.Credentials, error) {
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
// же, что у VK-провайдера, но сравнение регистронезависимое: текст приходит от
// разных слоёв стека и регистр не гарантирован.
//
// 403 намеренно НЕ в списке: TURN отвечает им на неразрешённый peer-адрес, а
// не на плохие креды, и сброс кеша по нему был бы ложным.
func (*Provider) IsAuthError(err error) bool {
	if err == nil {
		return false
	}
	s := strings.ToLower(err.Error())
	for _, needle := range []string{
		"401", "unauthorized", "authentication", "invalid credential", "stale nonce",
	} {
		if strings.Contains(s, needle) {
			return true
		}
	}
	return false
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

func (p *Provider) BackoffUntilUnix() int64 { return p.backoffUntil.Load() }

func (*Provider) Name() string { return "hub" }
