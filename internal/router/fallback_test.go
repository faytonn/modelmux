package router

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/faytonn/modelmux/internal/config"
	"github.com/faytonn/modelmux/internal/providers"
	"github.com/faytonn/modelmux/internal/resilience"
)

// mockCand builds a candidate backed by a mock provider. Lower price means the
// cost policy tries it earlier.
func mockCand(name string, price float64, timeout time.Duration, mc providers.MockConfig) Candidate {
	return Candidate{
		Provider: providers.NewMock(name, mc),
		Model:    config.Model{Name: "m", ContextWindow: 100_000, InputPricePer1K: price, OutputPricePer1K: price},
		Timeout:  timeout,
	}
}

func newTestRouter(t *testing.T, cands ...Candidate) *Router {
	t.Helper()
	return newTestRouterWithBreaker(t, config.Breaker{FailureThreshold: 100, OpenTimeout: time.Minute}, cands...)
}

func newTestRouterWithBreaker(t *testing.T, b config.Breaker, cands ...Candidate) *Router {
	t.Helper()
	r, err := New(cands, Options{
		Routing: config.Routing{Policy: "cost", LatencyWindow: 10},
		Retry:   config.Retry{MaxAttempts: 2, BaseDelay: time.Millisecond, MaxDelay: 5 * time.Millisecond, MaxRetryAfter: time.Second},
		Breaker: b,
		Logger:  discardLogger(),
	})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

var hello = Request{Messages: []providers.Message{{Role: "user", Content: "hello"}}, MaxTokens: 50}

func attemptKeys(res *Result) []string {
	var out []string
	for _, a := range res.Attempts {
		status := "ok"
		if a.Error != "" {
			status = "err"
		}
		out = append(out, a.Provider+":"+status)
	}
	return out
}

func TestFallsBackWhenPrimaryKeepsFailing(t *testing.T) {
	r := newTestRouter(t,
		mockCand("a", 0.001, 0, providers.MockConfig{ErrorRate: 1}),
		mockCand("b", 0.002, 0, providers.MockConfig{}),
	)
	res, err := r.Chat(context.Background(), hello)
	if err != nil {
		t.Fatal(err)
	}
	if res.Provider != "b" {
		t.Errorf("served by %q, want b", res.Provider)
	}
	// a is retried once (MaxAttempts 2), then b succeeds.
	if got := strings.Join(attemptKeys(res), ","); got != "a:err,a:err,b:ok" {
		t.Errorf("attempts = %s, want a:err,a:err,b:ok", got)
	}
}

func TestAttemptTimeoutTriggersFallback(t *testing.T) {
	r := newTestRouter(t,
		mockCand("slow", 0.001, 20*time.Millisecond, providers.MockConfig{Latency: time.Second}),
		mockCand("b", 0.002, 0, providers.MockConfig{}),
	)
	start := time.Now()
	res, err := r.Chat(context.Background(), hello)
	if err != nil {
		t.Fatal(err)
	}
	if res.Provider != "b" {
		t.Errorf("served by %q, want b", res.Provider)
	}
	if !strings.Contains(res.Attempts[0].Error, "deadline exceeded") {
		t.Errorf("first attempt error = %q, want a timeout", res.Attempts[0].Error)
	}
	if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
		t.Errorf("took %v; the 1s mock latency should have been cut off by the 20ms attempt timeout", elapsed)
	}
}

func TestLongRetryAfterFallsBackWithoutWaiting(t *testing.T) {
	r := newTestRouter(t,
		mockCand("limited", 0.001, 0, providers.MockConfig{RateLimitRate: 1, RetryAfter: 10 * time.Second}),
		mockCand("b", 0.002, 0, providers.MockConfig{}),
	)
	res, err := r.Chat(context.Background(), hello)
	if err != nil {
		t.Fatal(err)
	}
	// Retry-After (10s) exceeds MaxRetryAfter (1s), so no retry on "limited".
	if got := strings.Join(attemptKeys(res), ","); got != "limited:err,b:ok" {
		t.Errorf("attempts = %s, want limited:err,b:ok", got)
	}
}

func TestAllCandidatesFail(t *testing.T) {
	r := newTestRouter(t,
		mockCand("a", 0.001, 0, providers.MockConfig{ErrorRate: 1}),
		mockCand("b", 0.002, 0, providers.MockConfig{ErrorRate: 1}),
	)
	res, err := r.Chat(context.Background(), hello)
	if !errors.Is(err, ErrAllFailed) {
		t.Fatalf("err = %v, want ErrAllFailed", err)
	}
	if len(res.Attempts) != 4 {
		t.Errorf("attempts = %d, want 4 (2 per candidate)", len(res.Attempts))
	}
}

func TestRequestBudgetStopsTheChain(t *testing.T) {
	r := newTestRouter(t,
		mockCand("slow", 0.001, 0, providers.MockConfig{Latency: time.Second}),
		mockCand("b", 0.002, 0, providers.MockConfig{}),
	)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	res, err := r.Chat(ctx, hello)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want context.DeadlineExceeded", err)
	}
	if len(res.Attempts) != 1 {
		t.Errorf("attempts = %v, want only the slow one", attemptKeys(res))
	}
}

func TestOpenBreakerSkipsProviderWithoutCallingIt(t *testing.T) {
	r := newTestRouterWithBreaker(t, config.Breaker{FailureThreshold: 2, OpenTimeout: time.Minute},
		mockCand("a", 0.001, 0, providers.MockConfig{ErrorRate: 1}),
		mockCand("b", 0.002, 0, providers.MockConfig{}),
	)

	// First request: both attempts on a fail, which opens a's breaker; b serves.
	res, err := r.Chat(context.Background(), hello)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(attemptKeys(res), ","); got != "a:err,a:err,b:ok" {
		t.Fatalf("first request attempts = %s, want a:err,a:err,b:ok", got)
	}
	if s := r.breakers["a"].State(); s != resilience.Open {
		t.Fatalf("breaker for a is %v, want open", s)
	}

	// Second request: a is skipped instantly, no retries, straight to b.
	res, err = r.Chat(context.Background(), hello)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(attemptKeys(res), ","); got != "a:err,b:ok" {
		t.Errorf("second request attempts = %s, want a:err,b:ok", got)
	}
	if !strings.Contains(res.Attempts[0].Error, "circuit breaker open") {
		t.Errorf("first attempt error = %q, want circuit breaker open", res.Attempts[0].Error)
	}
}
