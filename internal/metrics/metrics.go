// Package metrics defines the gateway's Prometheus metrics.
//
// Labels are limited to values that come from config (provider, model) or a
// small fixed set (outcome), so label cardinality stays bounded no matter what
// clients send.
package metrics

import (
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
)

// Attempt outcomes, used as the "outcome" label.
const (
	OutcomeSuccess     = "success"
	OutcomeError       = "error"
	OutcomeRateLimited = "rate_limited"
	OutcomeTimeout     = "timeout"
	OutcomeBreakerOpen = "breaker_open"
	OutcomeCanceled    = "canceled"
)

// Metrics holds every collector. All methods are safe to call on a nil
// *Metrics, which records nothing; tests that don't care about metrics pass nil.
type Metrics struct {
	attempts     *prometheus.CounterVec
	latency      *prometheus.HistogramVec
	retries      *prometheus.CounterVec
	fallbacks    *prometheus.CounterVec
	cost         *prometheus.CounterVec
	tokens       *prometheus.CounterVec
	breakerState *prometheus.GaugeVec
	httpRequests *prometheus.CounterVec
	httpDuration *prometheus.HistogramVec
}

// New creates the metrics and registers them, plus Go runtime and process
// collectors, on reg.
func New(reg prometheus.Registerer) *Metrics {
	m := &Metrics{
		attempts: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "modelmux_upstream_requests_total",
			Help: "Upstream provider calls by outcome.",
		}, []string{"provider", "model", "outcome"}),
		latency: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "modelmux_upstream_request_duration_seconds",
			Help:    "Upstream provider call latency.",
			Buckets: []float64{.025, .05, .1, .25, .5, 1, 2.5, 5, 10, 30, 60},
		}, []string{"provider", "model"}),
		retries: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "modelmux_retries_total",
			Help: "Retries scheduled against a provider.",
		}, []string{"provider", "model"}),
		fallbacks: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "modelmux_fallbacks_total",
			Help: "Times the router gave up on this provider/model and moved to the next candidate.",
		}, []string{"provider", "model"}),
		cost: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "modelmux_estimated_cost_usd_total",
			Help: "Estimated spend from configured per-1K-token prices.",
		}, []string{"provider", "model"}),
		tokens: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "modelmux_tokens_total",
			Help: "Tokens processed, by direction (input or output).",
		}, []string{"provider", "model", "direction"}),
		breakerState: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "modelmux_circuit_breaker_state",
			Help: "Circuit breaker state per provider: 0 closed, 1 open, 2 half-open.",
		}, []string{"provider"}),
		httpRequests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "modelmux_http_requests_total",
			Help: "Requests to POST /v1/chat by HTTP status code.",
		}, []string{"code"}),
		httpDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "modelmux_http_request_duration_seconds",
			Help:    "End-to-end latency of POST /v1/chat, including retries and fallbacks.",
			Buckets: []float64{.05, .1, .25, .5, 1, 2.5, 5, 10, 30, 60},
		}, []string{"code"}),
	}
	reg.MustRegister(
		m.attempts, m.latency, m.retries, m.fallbacks, m.cost, m.tokens, m.breakerState,
		m.httpRequests, m.httpDuration,
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
	)
	return m
}

// ObserveAttempt records one upstream call. Calls rejected by an open breaker
// never reached the provider, so they are counted but not timed.
func (m *Metrics) ObserveAttempt(provider, model, outcome string, d time.Duration) {
	if m == nil {
		return
	}
	m.attempts.WithLabelValues(provider, model, outcome).Inc()
	if outcome != OutcomeBreakerOpen {
		m.latency.WithLabelValues(provider, model).Observe(d.Seconds())
	}
}

func (m *Metrics) ObserveRetry(provider, model string) {
	if m == nil {
		return
	}
	m.retries.WithLabelValues(provider, model).Inc()
}

func (m *Metrics) ObserveFallback(provider, model string) {
	if m == nil {
		return
	}
	m.fallbacks.WithLabelValues(provider, model).Inc()
}

// ObserveUsage records tokens and estimated cost for a successful call.
func (m *Metrics) ObserveUsage(provider, model string, inputTokens, outputTokens int, costUSD float64) {
	if m == nil {
		return
	}
	m.tokens.WithLabelValues(provider, model, "input").Add(float64(inputTokens))
	m.tokens.WithLabelValues(provider, model, "output").Add(float64(outputTokens))
	m.cost.WithLabelValues(provider, model).Add(costUSD)
}

// SetBreakerState records a breaker state: 0 closed, 1 open, 2 half-open.
func (m *Metrics) SetBreakerState(provider string, state int) {
	if m == nil {
		return
	}
	m.breakerState.WithLabelValues(provider).Set(float64(state))
}

func (m *Metrics) ObserveHTTP(code int, d time.Duration) {
	if m == nil {
		return
	}
	c := strconv.Itoa(code)
	m.httpRequests.WithLabelValues(c).Inc()
	m.httpDuration.WithLabelValues(c).Observe(d.Seconds())
}
