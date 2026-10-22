package providers

import (
	"context"
	"fmt"
	"math/rand/v2"
	"net/http"
	"sync"
	"time"
)

// MockConfig controls how a Mock behaves. ErrorRate and RateLimitRate are
// probabilities in [0, 1].
type MockConfig struct {
	Latency       time.Duration
	Jitter        time.Duration // extra random latency in [0, Jitter)
	ErrorRate     float64       // chance of a 503
	RateLimitRate float64       // chance of a 429
	RetryAfter    time.Duration // Retry-After sent with 429s
}

// Mock is a fake provider with configurable latency and failures, so the whole
// gateway can run and be tested without API keys.
type Mock struct {
	name string

	mu  sync.Mutex // guards cfg, which can be changed while requests are in flight
	cfg MockConfig
}

func NewMock(name string, cfg MockConfig) *Mock {
	return &Mock{name: name, cfg: cfg}
}

func (m *Mock) Name() string { return m.name }

func (m *Mock) Config() MockConfig {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.cfg
}

func (m *Mock) SetConfig(cfg MockConfig) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.cfg = cfg
}

func (m *Mock) Chat(ctx context.Context, req Request) (*Response, error) {
	cfg := m.Config()

	delay := cfg.Latency
	if cfg.Jitter > 0 {
		delay += rand.N(cfg.Jitter)
	}

	// Simulate the upstream call, but give up as soon as the caller's context
	// is cancelled or its deadline passes.
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-timer.C:
	}

	roll := rand.Float64()
	switch {
	case roll < cfg.RateLimitRate:
		return nil, &Error{Provider: m.name, StatusCode: http.StatusTooManyRequests, Retryable: true,
			RetryAfter: cfg.RetryAfter, Message: "mock rate limit"}
	case roll < cfg.RateLimitRate+cfg.ErrorRate:
		return nil, &Error{Provider: m.name, StatusCode: http.StatusServiceUnavailable, Retryable: true,
			Message: "mock upstream failure"}
	}

	content := fmt.Sprintf("[%s/%s] mock reply to: %q", m.name, req.Model, truncate(lastUserMessage(req.Messages), 60))
	out := min(EstimateTokens([]Message{{Content: content}}), req.MaxTokens)
	return &Response{Content: content, InputTokens: EstimateTokens(req.Messages), OutputTokens: out}, nil
}

func lastUserMessage(msgs []Message) string {
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role == "user" {
			return msgs[i].Content
		}
	}
	return ""
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "..."
}
