package router

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/faytonn/modelmux/internal/config"
	"github.com/faytonn/modelmux/internal/providers"
)

func discardLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func cand(provider, model string, contextWindow int, inPrice, outPrice float64, quality int) Candidate {
	return Candidate{
		Provider: providers.NewMock(provider, providers.MockConfig{}),
		Model: config.Model{Name: model, ContextWindow: contextWindow,
			InputPricePer1K: inPrice, OutputPricePer1K: outPrice, Quality: quality},
	}
}

func keys(cs []Candidate) []string {
	out := make([]string, len(cs))
	for i, c := range cs {
		out[i] = c.Key()
	}
	return out
}

// Deliberately not in any sorted order.
var testCands = []Candidate{
	cand("premium", "large", 200_000, 0.003, 0.015, 3),
	cand("cheap", "small", 8_000, 0.0002, 0.0008, 1),
	cand("mid", "medium", 32_000, 0.001, 0.003, 2),
}

type fakeLatency map[string]time.Duration

func (f fakeLatency) P95(provider string) (time.Duration, bool) {
	d, ok := f[provider]
	return d, ok
}

func TestPolicies(t *testing.T) {
	short := RouteInfo{PromptTokens: 100, MaxTokens: 256}
	long := RouteInfo{PromptTokens: 5000, MaxTokens: 256}
	rules := []config.Rule{{Name: "long", MinPromptTokens: 3000, Prefer: []string{"premium/large"}}}

	tests := []struct {
		name   string
		policy Policy
		info   RouteInfo
		want   []string
	}{
		{"cost picks cheapest first", CostPolicy{}, short,
			[]string{"cheap/small", "mid/medium", "premium/large"}},
		{"quality picks best first", QualityPolicy{}, short,
			[]string{"premium/large", "mid/medium", "cheap/small"}},
		{"latency picks lowest p95, unmeasured first",
			LatencyPolicy{Stats: fakeLatency{"premium": 50 * time.Millisecond, "cheap": 300 * time.Millisecond}}, short,
			[]string{"mid/medium", "premium/large", "cheap/small"}},
		{"latency ties broken by cost",
			LatencyPolicy{Stats: fakeLatency{"premium": time.Second, "cheap": time.Second, "mid": time.Second}}, short,
			[]string{"cheap/small", "mid/medium", "premium/large"}},
		{"rule: short prompt falls through to cost", RulePolicy{Rules: rules, Base: CostPolicy{}}, short,
			[]string{"cheap/small", "mid/medium", "premium/large"}},
		{"rule: long prompt prefers large context", RulePolicy{Rules: rules, Base: CostPolicy{}}, long,
			[]string{"premium/large", "cheap/small", "mid/medium"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := keys(tt.policy.Rank(tt.info, testCands))
			if !slices.Equal(got, tt.want) {
				t.Errorf("Rank() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestPolicyDoesNotMutateInput(t *testing.T) {
	before := keys(testCands)
	CostPolicy{}.Rank(RouteInfo{MaxTokens: 10}, testCands)
	if !slices.Equal(keys(testCands), before) {
		t.Fatal("Rank reordered its input slice")
	}
}

func TestFittingDropsSmallContextWindows(t *testing.T) {
	got := keys(fitting(RouteInfo{PromptTokens: 10_000, MaxTokens: 512}, testCands))
	want := []string{"premium/large", "mid/medium"}
	if !slices.Equal(got, want) {
		t.Errorf("fitting() = %v, want %v", got, want)
	}
}

func TestRouterHintOverridesDefaultPolicy(t *testing.T) {
	r, err := New(testCands, Options{Routing: config.Routing{Policy: "cost", LatencyWindow: 10}, Logger: discardLogger()})
	if err != nil {
		t.Fatal(err)
	}
	msgs := []providers.Message{{Role: "user", Content: "hi"}}

	res, err := r.Chat(context.Background(), Request{Messages: msgs, MaxTokens: 50})
	if err != nil {
		t.Fatal(err)
	}
	if res.Model != "small" || res.Policy != "cost" {
		t.Errorf("no hint: got %s via %s, want small via cost", res.Model, res.Policy)
	}

	res, err = r.Chat(context.Background(), Request{Messages: msgs, MaxTokens: 50, Hint: "quality"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Model != "large" || res.Policy != "quality" {
		t.Errorf("quality hint: got %s via %s, want large via quality", res.Model, res.Policy)
	}
}

func TestRouterRejectsPromptLargerThanEveryModel(t *testing.T) {
	r, err := New(testCands, Options{Routing: config.Routing{Policy: "cost", LatencyWindow: 10}, Logger: discardLogger()})
	if err != nil {
		t.Fatal(err)
	}
	huge := strings.Repeat("x", 1_000_000) // ~250K tokens
	_, err = r.Chat(context.Background(), Request{Messages: []providers.Message{{Role: "user", Content: huge}}, MaxTokens: 50})
	if !errors.Is(err, ErrNoCandidates) {
		t.Fatalf("err = %v, want ErrNoCandidates", err)
	}
}
