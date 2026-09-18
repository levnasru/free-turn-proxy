package stunprobe

import (
	"context"
	"sort"
	"sync"
	"time"

	"github.com/samosvalishe/free-turn-proxy/internal/logx"
	"github.com/samosvalishe/free-turn-proxy/internal/proxy/common"
)

type cacheEntry struct {
	rtt      time.Duration
	probedAt time.Time
}

// Config configures the Ranker.
type Config struct {
	TTL              time.Duration // how long probe results remain valid (default: 15 min)
	Timeout          time.Duration // probe UDP timeout per candidate (default: 300 ms)
	Concurrency      int           // max parallel probe goroutines (default: 20)
	MinClusterSize   int           // minimum servers required in the fast cluster (default: 5)
	ToleranceAbsolute time.Duration // absolute latency delta to consider homogeneous (default: 20 ms)
	ToleranceRatio   float64       // ratio over minRTT to consider homogeneous (default: 1.8)
	Log              logx.Logger
}

// DefaultConfig returns safe production defaults.
func DefaultConfig() Config {
	return Config{
		TTL:               15 * time.Minute,
		Timeout:           1200 * time.Millisecond,
		Concurrency:       12,
		MinClusterSize:    5,
		ToleranceAbsolute: 20 * time.Millisecond,
		ToleranceRatio:    1.6,
		Log:               logx.Nop(),
	}
}

// Ranker dynamically probes and filters candidate TURN relays by client-measured RTT.
type Ranker struct {
	cfg Config

	mu      sync.RWMutex
	probeMu sync.Mutex
	cache   map[string]cacheEntry
}

// NewRanker creates a new Ranker with the given configuration.
func NewRanker(cfg Config) *Ranker {
	def := DefaultConfig()
	if cfg.TTL <= 0 {
		cfg.TTL = def.TTL
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = def.Timeout
	}
	if cfg.Concurrency <= 0 {
		cfg.Concurrency = def.Concurrency
	}
	if cfg.MinClusterSize <= 0 {
		cfg.MinClusterSize = def.MinClusterSize
	}
	if cfg.ToleranceAbsolute <= 0 {
		cfg.ToleranceAbsolute = def.ToleranceAbsolute
	}
	if cfg.ToleranceRatio <= 0 {
		cfg.ToleranceRatio = def.ToleranceRatio
	}
	if cfg.Log == nil {
		cfg.Log = def.Log
	}

	return &Ranker{
		cfg:   cfg,
		cache: make(map[string]cacheEntry),
	}
}

// Invalidate clears all cached probe measurements (e.g. on network handover).
func (r *Ranker) Invalidate() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.cache = make(map[string]cacheEntry)
	r.cfg.Log.Infof("[STUNProbe] Cache invalidated (network handover)")
}

// FilterHomogeneous probes candidates not yet in cache, determines the lowest-latency
// cluster, and returns only relays whose latency falls within the homogeneous envelope.
func (r *Ranker) FilterHomogeneous(ctx context.Context, candidates []string) []string {
	if len(candidates) <= 1 {
		return candidates
	}

	now := time.Now()
	var toProbe []string

	r.mu.RLock()
	for _, c := range candidates {
		entry, ok := r.cache[c]
		if !ok || now.Sub(entry.probedAt) > r.cfg.TTL {
			toProbe = append(toProbe, c)
		}
	}
	r.mu.RUnlock()

	// Probe any uncached or stale candidates with a single-flight mutex so concurrent streams don't hammer the network
	if len(toProbe) > 0 {
		r.probeMu.Lock()
		// Re-check under probeMu: another stream may have completed probing while we waited
		now = time.Now()
		var stillNeeded []string
		r.mu.RLock()
		for _, c := range toProbe {
			entry, ok := r.cache[c]
			if !ok || now.Sub(entry.probedAt) > r.cfg.TTL {
				stillNeeded = append(stillNeeded, c)
			}
		}
		r.mu.RUnlock()

		if len(stillNeeded) > 0 {
			r.cfg.Log.Debugf("[STUNProbe] Probing %d new/stale TURN candidates (timeout=%s, concurrency=%d)", len(stillNeeded), r.cfg.Timeout, r.cfg.Concurrency)
			probed := ProbeAll(ctx, stillNeeded, r.cfg.Timeout, r.cfg.Concurrency)
			r.mu.Lock()
			for target, rtt := range probed {
				r.cache[target] = cacheEntry{rtt: rtt, probedAt: now}
			}
			// Only cache negative RTT if at least some servers responded.
			// If len(probed) == 0, the radio was waking up or network was temporarily down; DO NOT poison cache with -1!
			if len(probed) > 0 {
				for _, target := range stillNeeded {
					if _, ok := probed[target]; !ok {
						r.cache[target] = cacheEntry{rtt: -1, probedAt: now}
					}
				}
			}
			r.mu.Unlock()
		}
		r.probeMu.Unlock()
	}

	// Read RTTs for all candidates
	type scoredCandidate struct {
		addr string
		rtt  time.Duration
	}

	var responsive []scoredCandidate
	var minRTT time.Duration = -1

	r.mu.RLock()
	for _, c := range candidates {
		entry, ok := r.cache[c]
		if ok && entry.rtt > 0 {
			responsive = append(responsive, scoredCandidate{addr: c, rtt: entry.rtt})
			if minRTT < 0 || entry.rtt < minRTT {
				minRTT = entry.rtt
			}
		}
	}
	r.mu.RUnlock()

	// If no candidate responded, gracefully fall back to original candidates list
	if len(responsive) == 0 || minRTT <= 0 {
		r.cfg.Log.Warnf("[STUNProbe] No TURN candidates responded to STUN probe; using unranked list (%d candidates)", len(candidates))
		return candidates
	}

	// Calculate latency envelope cutoff:
	// Envelope allows at least minRTT * ToleranceRatio OR minRTT + ToleranceAbsolute, whichever is larger.
	ratioCutoff := time.Duration(float64(minRTT) * r.cfg.ToleranceRatio)
	absCutoff := minRTT + r.cfg.ToleranceAbsolute
	cutoff := ratioCutoff
	if absCutoff > cutoff {
		cutoff = absCutoff
	}

	var fast []scoredCandidate
	var slow []scoredCandidate

	for _, sc := range responsive {
		if sc.rtt <= cutoff {
			fast = append(fast, sc)
		} else {
			slow = append(slow, sc)
		}
	}

	sort.Slice(fast, func(i, j int) bool {
		return fast[i].rtt < fast[j].rtt
	})

	// If fast cluster has enough servers, return only fast cluster to guarantee minimal out-of-order latency skew
	if len(fast) >= r.cfg.MinClusterSize {
		r.cfg.Log.Infof("[STUNProbe] Clustered %d/%d responsive candidates into homogeneous zone (minRTT=%.1fms, cutoff=%.1fms, dropped %d slow relays)",
			len(fast), len(responsive), float64(minRTT.Microseconds())/1000.0, float64(cutoff.Microseconds())/1000.0, len(slow))
		out := make([]string, len(fast))
		for i, f := range fast {
			out[i] = f.addr
		}
		return out
	}

	// If fast cluster has at least 3 servers, return fast cluster.
	// 3 servers are more than enough to multiplex streams with path diversity while avoiding 100ms+ tail relays.
	if len(fast) >= 3 {
		r.cfg.Log.Infof("[STUNProbe] Using %d fast candidates (minRTT=%.1fms, cutoff=%.1fms, below MinClusterSize %d)",
			len(fast), float64(minRTT.Microseconds())/1000.0, float64(cutoff.Microseconds())/1000.0, r.cfg.MinClusterSize)
		out := make([]string, len(fast))
		for i, f := range fast {
			out[i] = f.addr
		}
		return out
	}

	// Fallback if fast cluster is extremely small (<3): return top responsive candidates sorted ascending by RTT,
	// capped to top 20 to avoid pulling in high-latency tail relays (>100ms).
	sort.Slice(responsive, func(i, j int) bool {
		return responsive[i].rtt < responsive[j].rtt
	})
	maxFallback := 20
	if len(responsive) < maxFallback {
		maxFallback = len(responsive)
	}
	r.cfg.Log.Warnf("[STUNProbe] Fast cluster too small (%d < %d); returning top %d/%d responsive candidates sorted by RTT (minRTT=%.1fms, maxRTT=%.1fms)",
		len(fast), r.cfg.MinClusterSize, maxFallback, len(responsive), float64(minRTT.Microseconds())/1000.0, float64(responsive[maxFallback-1].rtt.Microseconds())/1000.0)

	out := make([]string, maxFallback)
	for i := 0; i < maxFallback; i++ {
		out[i] = responsive[i].addr
	}
	return out
}

// WrapGetCreds decorates a common.GetCredsFunc to dynamically filter candidate TURN relays.
func (r *Ranker) WrapGetCreds(getCreds common.GetCredsFunc) common.GetCredsFunc {
	return func(ctx context.Context, streamID int) (user, pass string, rawURLs []string, err error) {
		user, pass, rawURLs, err = getCreds(ctx, streamID)
		if err != nil || len(rawURLs) <= 1 {
			return user, pass, rawURLs, err
		}
		ranked := r.FilterHomogeneous(ctx, rawURLs)
		return user, pass, ranked, nil
	}
}
