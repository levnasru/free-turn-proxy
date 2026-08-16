// Package multi объединяет несколько provider.Provider в один, распределяя
// streamID по вложенным провайдерам round-robin. Применение: несколько
// VK-звонков как единый пул TURN-стримов (больше параллельных аллокаций).
package multi

import (
	"context"
	"fmt"
	"time"

	"github.com/samosvalishe/free-turn-proxy/internal/provider"
)

// Provider распределяет 1-based streamID round-robin: базовый провайдер = (streamID-1) % n,
// внутренний streamID = ((streamID-1) / n) + 1 (биекция, без коллизий) - пока
// базовый провайдер живой. Если у него активен backoff (BackoffUntilUnix() в
// будущем - hub недоступен/в фатальном auth-цикле), providerFor обходит
// остальных по кругу и отдаёт первого живого; innerID при этом не пересчитывается
// (не критично: реальные реализации, hub.Provider, не хранят состояние по
// streamID для логики, только для логов).
// Без изменяемого состояния - thread-safe.
type Provider struct {
	providers []provider.Provider
	n         int
}

func New(providers []provider.Provider) *Provider {
	if len(providers) == 0 {
		panic("multi: empty providers")
	}
	return &Provider{providers: providers, n: len(providers)}
}

func (m *Provider) providerFor(streamID int) (provider.Provider, int) {
	idx := (streamID - 1) % m.n
	innerID := ((streamID - 1) / m.n) + 1
	now := time.Now().Unix()
	for i := 0; i < m.n; i++ {
		cand := (idx + i) % m.n
		if until := m.providers[cand].BackoffUntilUnix(); until == 0 || until < now {
			return m.providers[cand], innerID
		}
	}
	// все в backoff - возвращаем исходный, пусть ошибка всплывёт как обычно.
	return m.providers[idx], innerID
}

func (m *Provider) GetCredentials(ctx context.Context, streamID int) (provider.Credentials, error) {
	p, innerID := m.providerFor(streamID)
	return p.GetCredentials(ctx, innerID)
}

// IsAuthError делегирует первому провайдеру: все вложенные однотипны (VK).
func (m *Provider) IsAuthError(err error) bool {
	return m.providers[0].IsAuthError(err)
}

func (m *Provider) HandleAuthError(streamID int) bool {
	p, innerID := m.providerFor(streamID)
	return p.HandleAuthError(innerID)
}

func (m *Provider) ResetErrors(streamID int) {
	p, innerID := m.providerFor(streamID)
	p.ResetErrors(innerID)
}

// BackoffUntilUnix - максимум среди провайдеров (один в блокировке -> пауза для всех).
func (m *Provider) BackoffUntilUnix() int64 {
	var until int64
	for _, p := range m.providers {
		if v := p.BackoffUntilUnix(); v > until {
			until = v
		}
	}
	return until
}

func (m *Provider) Name() string {
	return fmt.Sprintf("multi(%d)", m.n)
}
