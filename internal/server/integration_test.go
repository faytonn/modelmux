package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/faytonn/modelmux/internal/config"
	"github.com/faytonn/modelmux/internal/metrics"
	"github.com/faytonn/modelmux/internal/providers"
	"github.com/faytonn/modelmux/internal/router"
)

// newGateway starts the full HTTP stack (middleware, router, retries,
// breakers, metrics) in front of two mock providers: "a" is cheaper, so the
// cost policy always tries it first, and "b" is the fallback.
func newGateway(t *testing.T, a, b providers.MockConfig) *httptest.Server {
	t.Helper()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	mockA, mockB := providers.NewMock("a", a), providers.NewMock("b", b)
	cands := []router.Candidate{
		{Provider: mockA, Model: config.Model{Name: "a-model", ContextWindow: 8000, InputPricePer1K: 0.001, OutputPricePer1K: 0.001}},
		{Provider: mockB, Model: config.Model{Name: "b-model", ContextWindow: 8000, InputPricePer1K: 0.01, OutputPricePer1K: 0.01}},
	}

	reg := prometheus.NewRegistry()
	m := metrics.New(reg)
	rt, err := router.New(cands, router.Options{
		Routing: config.Routing{Policy: "cost", LatencyWindow: 10},
		Retry:   config.Retry{MaxAttempts: 2, BaseDelay: time.Millisecond, MaxDelay: 5 * time.Millisecond, MaxRetryAfter: time.Second},
		Breaker: config.Breaker{FailureThreshold: 5, OpenTimeout: time.Minute},
		Metrics: m,
		Logger:  log,
	})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(New(Options{
		Router:         rt,
		Config:         config.Server{RequestTimeout: 5 * time.Second, DefaultMaxTokens: 64},
		Metrics:        m,
		MetricsHandler: promhttp.HandlerFor(reg, promhttp.HandlerOpts{}),
		Mocks:          map[string]*providers.Mock{"a": mockA, "b": mockB},
		Logger:         log,
	}).Handler())
	t.Cleanup(srv.Close)
	return srv
}

func postChat(t *testing.T, srv *httptest.Server, body string) (*http.Response, map[string]any) {
	t.Helper()
	resp, err := http.Post(srv.URL+"/v1/chat", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	return resp, out
}

func getMetrics(t *testing.T, srv *httptest.Server) string {
	t.Helper()
	resp, err := http.Get(srv.URL + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b)
}

const hi = `{"messages":[{"role":"user","content":"hi"}]}`

// The required end-to-end scenario: provider A fails, the request falls back
// to B and succeeds, and the fallback is visible in the response and metrics.
func TestIntegrationFallbackToSecondProvider(t *testing.T) {
	srv := newGateway(t, providers.MockConfig{ErrorRate: 1}, providers.MockConfig{})

	resp, body := postChat(t, srv, hi)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body = %v", resp.StatusCode, body)
	}
	if body["provider"] != "b" || body["model"] != "b-model" {
		t.Errorf("served by %v/%v, want b/b-model", body["provider"], body["model"])
	}
	attempts := body["attempts"].([]any)
	if len(attempts) != 3 { // a, a (retry), b
		t.Errorf("attempts = %v, want 3", attempts)
	}
	if id := resp.Header.Get("X-Request-ID"); id == "" || id != body["request_id"] {
		t.Errorf("X-Request-ID header %q does not match body request_id %v", id, body["request_id"])
	}

	m := getMetrics(t, srv)
	for _, want := range []string{
		`modelmux_fallbacks_total{model="a-model",provider="a"} 1`,
		`modelmux_upstream_requests_total{model="a-model",outcome="error",provider="a"} 2`,
		`modelmux_upstream_requests_total{model="b-model",outcome="success",provider="b"} 1`,
		`modelmux_http_requests_total{code="200"} 1`,
	} {
		if !strings.Contains(m, want) {
			t.Errorf("metrics missing %q", want)
		}
	}
}

func TestIntegrationAllProvidersFail(t *testing.T) {
	srv := newGateway(t, providers.MockConfig{ErrorRate: 1}, providers.MockConfig{ErrorRate: 1})

	resp, body := postChat(t, srv, hi)
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", resp.StatusCode)
	}
	if !strings.Contains(body["error"].(string), "all providers failed") || len(body["attempts"].([]any)) != 4 {
		t.Errorf("body = %v, want all-failed error with 4 attempts", body)
	}
}

func TestIntegrationAdminOutageAndRecovery(t *testing.T) {
	srv := newGateway(t, providers.MockConfig{}, providers.MockConfig{})

	patch := func(body string) {
		req, _ := http.NewRequest(http.MethodPatch, srv.URL+"/admin/mocks/a", strings.NewReader(body))
		resp, err := http.DefaultClient.Do(req)
		if err != nil || resp.StatusCode != http.StatusOK {
			t.Fatalf("PATCH %s: %v %v", body, err, resp.Status)
		}
		resp.Body.Close()
	}

	if _, body := postChat(t, srv, hi); body["provider"] != "a" {
		t.Fatalf("healthy: served by %v, want a", body["provider"])
	}
	patch(`{"error_rate": 1}`)
	if _, body := postChat(t, srv, hi); body["provider"] != "b" {
		t.Fatalf("outage: served by %v, want b", body["provider"])
	}
	patch(`{"error_rate": 0}`)
	if _, body := postChat(t, srv, hi); body["provider"] != "a" {
		t.Fatalf("recovered: served by %v, want a", body["provider"])
	}
}

func TestChatValidation(t *testing.T) {
	srv := newGateway(t, providers.MockConfig{}, providers.MockConfig{})
	tests := []struct {
		name string
		body string
	}{
		{"not JSON", `{`},
		{"no messages", `{"messages":[]}`},
		{"bad role", `{"messages":[{"role":"robot","content":"hi"}]}`},
		{"bad hint", `{"messages":[{"role":"user","content":"hi"}],"routing_hint":"turbo"}`},
		{"unknown field", `{"messages":[{"role":"user","content":"hi"}],"temperature":2}`},
		{"negative max_tokens", `{"messages":[{"role":"user","content":"hi"}],"max_tokens":-1}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resp, body := postChat(t, srv, tt.body)
			if resp.StatusCode != http.StatusBadRequest || body["error"] == "" {
				t.Errorf("status = %d, body = %v; want 400 with an error", resp.StatusCode, body)
			}
		})
	}

	resp, err := http.Get(srv.URL + "/v1/chat")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("GET /v1/chat = %d, want 405", resp.StatusCode)
	}
}

func TestRequestIDFromClientIsKeptOnlyIfSafe(t *testing.T) {
	srv := newGateway(t, providers.MockConfig{}, providers.MockConfig{})
	send := func(id string) string {
		req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/chat", strings.NewReader(hi))
		req.Header.Set("X-Request-ID", id)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.Header.Get("X-Request-ID")
	}
	if got := send("trace-123"); got != "trace-123" {
		t.Errorf("safe ID replaced: got %q", got)
	}
	if got := send("bad id with spaces"); got == "bad id with spaces" || got == "" {
		t.Errorf("unsafe ID kept or empty: got %q", got)
	}
}

func TestStatusFor(t *testing.T) {
	tests := []struct {
		err  error
		want int
	}{
		{fmt.Errorf("request budget exhausted: %w", context.DeadlineExceeded), http.StatusGatewayTimeout},
		{fmt.Errorf("request budget exhausted: %w", context.Canceled), statusClientClosedRequest},
		{router.ErrNoCandidates, http.StatusUnprocessableEntity},
		{fmt.Errorf("%w: last error: boom", router.ErrAllFailed), http.StatusBadGateway},
	}
	for _, tt := range tests {
		if got := statusFor(tt.err); got != tt.want {
			t.Errorf("statusFor(%v) = %d, want %d", tt.err, got, tt.want)
		}
	}
}
