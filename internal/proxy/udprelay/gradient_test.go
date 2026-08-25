package udprelay

import (
	"testing"
	"time"
)

func TestSuggestedHotSetSizeNoData(t *testing.T) {
	t.Parallel()
	if _, got := suggestedHotSetSize(30, 0, 0); got != 30 {
		t.Fatalf("expected no-op (currentK) with zero RTTs, got %d", got)
	}
	if _, got := suggestedHotSetSize(30, 16*time.Millisecond, 0); got != 30 {
		t.Fatalf("expected no-op with zero baseline, got %d", got)
	}
}

func TestSuggestedHotSetSizeHealthyGrows(t *testing.T) {
	t.Parallel()
	// avgRTT близко к baseline (данные из живого лога 2026-08-25,
	// download-фаза: minRTT~12ms, avgRTT~16ms) - gradient чуть ниже 1,
	// предложение растёт от headroom, не падает камнем.
	gradient, suggested := suggestedHotSetSize(30, 16*time.Millisecond, 12*time.Millisecond)
	if gradient < 0.85 || gradient > 1.0 {
		t.Fatalf("expected gradient near 0.9 for near-baseline RTT, got %.3f", gradient)
	}
	if suggested <= 30 {
		t.Fatalf("expected growth suggestion above current K=30 when near baseline, got %d", suggested)
	}
}

func TestSuggestedHotSetSizeDegradedShrinks(t *testing.T) {
	t.Parallel()
	// upload-фаза того же лога: avgRTT потянут вверх (~22ms) относительно
	// baseline (~12ms) - gradient заметно ниже 1, предложение ниже текущего K.
	gradient, suggested := suggestedHotSetSize(30, 22*time.Millisecond, 12*time.Millisecond)
	if gradient >= 0.85 {
		t.Fatalf("expected gradient well below 1 for degraded RTT, got %.3f", gradient)
	}
	if suggested >= 30 {
		t.Fatalf("expected shrink suggestion below current K=30 when degraded, got %d", suggested)
	}
}

func TestSuggestedHotSetSizeClampedToUpperBound(t *testing.T) {
	t.Parallel()
	// avgRTT много ниже baseline (не бывает физически, но входные данные не
	// проверяются) - gradient>1 не должен улетать в небо без границы.
	_, suggested := suggestedHotSetSize(10, 1*time.Millisecond, 12*time.Millisecond)
	if suggested > 30 {
		t.Fatalf("expected suggestion clamped to currentK*3=30, got %d", suggested)
	}
}

func TestGradientTrackerResetsWindow(t *testing.T) {
	t.Parallel()
	g := newGradientTracker()
	g.observe(20 * time.Millisecond)
	g.observe(10 * time.Millisecond)
	if got := g.baseline(); got != 10*time.Millisecond {
		t.Fatalf("expected baseline to track the minimum observed, got %s", got)
	}

	// Форсируем истёкшее окно, минуя таймер.
	g.windowStart = time.Now().Add(-minRTTResetInterval - time.Second)
	g.observe(15 * time.Millisecond)
	if got := g.baseline(); got != 15*time.Millisecond {
		t.Fatalf("expected baseline to reset after window expiry, got %s", got)
	}
}
