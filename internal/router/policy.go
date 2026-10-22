package router

import (
	"cmp"
	"slices"
	"time"

	"github.com/faytonn/modelmux/internal/config"
)

// RouteInfo is what a policy knows about a request.
type RouteInfo struct {
	PromptTokens int // estimated
	MaxTokens    int
}

// Policy orders candidates for a request. The first candidate is the primary
// choice; the rest form the fallback chain. Policies never call providers, so
// they are cheap to run and easy to test.
type Policy interface {
	Name() string
	Rank(info RouteInfo, cands []Candidate) []Candidate
}

// estimatedCost assumes the full max_tokens are generated, so it is an upper bound.
func estimatedCost(c Candidate, info RouteInfo) float64 {
	return c.Model.Cost(info.PromptTokens, info.MaxTokens)
}

// CostPolicy prefers the cheapest model.
type CostPolicy struct{}

func (CostPolicy) Name() string { return "cost" }

func (CostPolicy) Rank(info RouteInfo, cands []Candidate) []Candidate {
	out := slices.Clone(cands)
	slices.SortStableFunc(out, func(a, b Candidate) int {
		return cmp.Compare(estimatedCost(a, info), estimatedCost(b, info))
	})
	return out
}

// QualityPolicy prefers the highest-quality model, then the cheapest.
type QualityPolicy struct{}

func (QualityPolicy) Name() string { return "quality" }

func (QualityPolicy) Rank(info RouteInfo, cands []Candidate) []Candidate {
	out := slices.Clone(cands)
	slices.SortStableFunc(out, func(a, b Candidate) int {
		return cmp.Or(
			cmp.Compare(b.Model.Quality, a.Model.Quality), // descending
			cmp.Compare(estimatedCost(a, info), estimatedCost(b, info)),
		)
	})
	return out
}

// LatencySource reports recent p95 latency per provider. It is defined here,
// where it is used, so tests can pass a simple fake.
type LatencySource interface {
	P95(provider string) (time.Duration, bool)
}

// LatencyPolicy prefers the provider with the lowest recent p95 latency.
// Providers with no samples yet sort first (treated as 0), so every provider
// gets measured instead of being starved by an early winner.
type LatencyPolicy struct {
	Stats LatencySource
}

func (LatencyPolicy) Name() string { return "latency" }

func (p LatencyPolicy) Rank(info RouteInfo, cands []Candidate) []Candidate {
	p95 := func(c Candidate) time.Duration {
		d, _ := p.Stats.P95(c.Provider.Name())
		return d
	}
	out := slices.Clone(cands)
	slices.SortStableFunc(out, func(a, b Candidate) int {
		return cmp.Or(
			cmp.Compare(p95(a), p95(b)),
			cmp.Compare(estimatedCost(a, info), estimatedCost(b, info)),
		)
	})
	return out
}

// RulePolicy applies the first matching rule from config: its preferred models
// go to the front, in the listed order, followed by everything else in Base order.
type RulePolicy struct {
	Rules []config.Rule
	Base  Policy
}

func (RulePolicy) Name() string { return "rule" }

func (p RulePolicy) Rank(info RouteInfo, cands []Candidate) []Candidate {
	ranked := p.Base.Rank(info, cands)
	for _, rule := range p.Rules {
		if info.PromptTokens >= rule.MinPromptTokens {
			return preferFirst(ranked, rule.Prefer)
		}
	}
	return ranked
}

func preferFirst(ranked []Candidate, keys []string) []Candidate {
	out := make([]Candidate, 0, len(ranked))
	used := make(map[string]bool, len(keys))
	for _, k := range keys {
		for _, c := range ranked {
			if c.Key() == k && !used[k] {
				out = append(out, c)
				used[k] = true
			}
		}
	}
	for _, c := range ranked {
		if !used[c.Key()] {
			out = append(out, c)
		}
	}
	return out
}

// fitting drops candidates whose context window cannot hold the prompt plus
// the requested output.
func fitting(info RouteInfo, cands []Candidate) []Candidate {
	var out []Candidate
	for _, c := range cands {
		if info.PromptTokens+info.MaxTokens <= c.Model.ContextWindow {
			out = append(out, c)
		}
	}
	return out
}
