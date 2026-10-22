// Package router decides which provider and model serve a request.
package router

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/faytonn/modelmux/internal/config"
	"github.com/faytonn/modelmux/internal/providers"
)

// ErrNoCandidates means no configured model can serve the request.
var ErrNoCandidates = errors.New("no model can serve this request")

// Candidate is one (provider, model) pair the router can send a request to.
type Candidate struct {
	Provider providers.Provider
	Model    config.Model
}

// Key identifies a candidate as "provider/model".
func (c Candidate) Key() string { return c.Provider.Name() + "/" + c.Model.Name }

// Request is what the router receives from the HTTP layer.
type Request struct {
	Messages  []providers.Message
	MaxTokens int
	Hint      string // optional: "cheap", "fast" or "quality"
}

// Attempt records one upstream call, for the response and for debugging.
type Attempt struct {
	Provider string `json:"provider"`
	Model    string `json:"model"`
	Error    string `json:"error,omitempty"`
}

// Result is the outcome of a routed request.
type Result struct {
	Provider string
	Model    string
	Policy   string
	Response *providers.Response
	CostUSD  float64
	Attempts []Attempt
}

// hintPolicies maps a request's routing_hint to a policy name.
var hintPolicies = map[string]string{"cheap": "cost", "fast": "latency", "quality": "quality"}

type Router struct {
	candidates    []Candidate
	policies      map[string]Policy
	defaultPolicy Policy
	latency       *LatencyTracker
	log           *slog.Logger
}

func New(candidates []Candidate, cfg config.Routing, log *slog.Logger) (*Router, error) {
	latency := NewLatencyTracker(cfg.LatencyWindow)
	policies := map[string]Policy{
		"cost":    CostPolicy{},
		"latency": LatencyPolicy{Stats: latency},
		"quality": QualityPolicy{},
		"rule":    RulePolicy{Rules: cfg.Rules, Base: CostPolicy{}},
	}
	def, ok := policies[cfg.Policy]
	if !ok {
		return nil, fmt.Errorf("unknown routing policy %q", cfg.Policy)
	}
	return &Router{candidates: candidates, policies: policies, defaultPolicy: def, latency: latency, log: log}, nil
}

// plan picks the policy for this request and returns the ordered candidates.
func (r *Router) plan(req Request) (Policy, []Candidate) {
	policy := r.defaultPolicy
	if name, ok := hintPolicies[req.Hint]; ok {
		policy = r.policies[name]
	}
	info := RouteInfo{PromptTokens: providers.EstimateTokens(req.Messages), MaxTokens: req.MaxTokens}
	return policy, policy.Rank(info, fitting(info, r.candidates))
}

// Chat sends the request to the top-ranked candidate. Retries and fallbacks
// are layered on in the next step.
func (r *Router) Chat(ctx context.Context, req Request) (*Result, error) {
	policy, ranked := r.plan(req)
	if len(ranked) == 0 {
		return nil, ErrNoCandidates
	}
	c := ranked[0]
	r.log.Debug("routing", "policy", policy.Name(), "candidate", c.Key())

	start := time.Now()
	resp, err := c.Provider.Chat(ctx, providers.Request{Model: c.Model.Name, Messages: req.Messages, MaxTokens: req.MaxTokens})
	attempt := Attempt{Provider: c.Provider.Name(), Model: c.Model.Name}
	if err != nil {
		attempt.Error = err.Error()
		return &Result{Policy: policy.Name(), Attempts: []Attempt{attempt}}, err
	}
	r.latency.Observe(c.Provider.Name(), time.Since(start))
	return &Result{
		Provider: c.Provider.Name(),
		Model:    c.Model.Name,
		Policy:   policy.Name(),
		Response: resp,
		CostUSD:  c.Model.Cost(resp.InputTokens, resp.OutputTokens),
		Attempts: []Attempt{attempt},
	}, nil
}
