package router

import (
	"slices"
	"sync"
	"time"
)

// LatencyTracker keeps the last N successful call latencies per provider and
// computes p95 over them. A fixed-size window means old samples age out, so
// the policy reacts when a provider gets slower or recovers.
type LatencyTracker struct {
	window int

	mu      sync.Mutex
	samples map[string]*ring
}

type ring struct {
	buf  []time.Duration
	next int
}

func NewLatencyTracker(window int) *LatencyTracker {
	return &LatencyTracker{window: window, samples: map[string]*ring{}}
}

func (t *LatencyTracker) Observe(provider string, d time.Duration) {
	t.mu.Lock()
	defer t.mu.Unlock()
	r, ok := t.samples[provider]
	if !ok {
		r = &ring{buf: make([]time.Duration, 0, t.window)}
		t.samples[provider] = r
	}
	if len(r.buf) < t.window {
		r.buf = append(r.buf, d)
		return
	}
	r.buf[r.next] = d
	r.next = (r.next + 1) % t.window
}

// P95 returns the nearest-rank 95th percentile, or false with no samples.
func (t *LatencyTracker) P95(provider string) (time.Duration, bool) {
	t.mu.Lock()
	r, ok := t.samples[provider]
	if !ok || len(r.buf) == 0 {
		t.mu.Unlock()
		return 0, false
	}
	sorted := slices.Clone(r.buf)
	t.mu.Unlock() // sort outside the lock; the copy is ours

	slices.Sort(sorted)
	idx := (len(sorted)*95+99)/100 - 1 // ceil(0.95*n) - 1
	return sorted[idx], true
}
