package router

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/faytonn/modelmux/internal/config"
	"github.com/faytonn/modelmux/internal/metrics"
	"github.com/faytonn/modelmux/internal/providers"
)

func TestRouterRecordsMetrics(t *testing.T) {
	reg := prometheus.NewRegistry()
	r, err := New([]Candidate{
		mockCand("a", 0.001, 0, providers.MockConfig{ErrorRate: 1}),
		mockCand("b", 0.002, 0, providers.MockConfig{}),
	}, Options{
		Routing: config.Routing{Policy: "cost", LatencyWindow: 10},
		Retry:   config.Retry{MaxAttempts: 2, BaseDelay: time.Millisecond, MaxDelay: time.Millisecond},
		Breaker: config.Breaker{FailureThreshold: 100, OpenTimeout: time.Minute},
		Metrics: metrics.New(reg),
		Logger:  discardLogger(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.Chat(context.Background(), hello); err != nil {
		t.Fatal(err)
	}

	want := `
# HELP modelmux_fallbacks_total Times the router gave up on this provider/model and moved to the next candidate.
# TYPE modelmux_fallbacks_total counter
modelmux_fallbacks_total{model="m",provider="a"} 1
# HELP modelmux_retries_total Retries scheduled against a provider.
# TYPE modelmux_retries_total counter
modelmux_retries_total{model="m",provider="a"} 1
# HELP modelmux_upstream_requests_total Upstream provider calls by outcome.
# TYPE modelmux_upstream_requests_total counter
modelmux_upstream_requests_total{model="m",outcome="error",provider="a"} 2
modelmux_upstream_requests_total{model="m",outcome="success",provider="b"} 1
`
	if err := testutil.GatherAndCompare(reg, strings.NewReader(want),
		"modelmux_fallbacks_total", "modelmux_retries_total", "modelmux_upstream_requests_total"); err != nil {
		t.Error(err)
	}
}
