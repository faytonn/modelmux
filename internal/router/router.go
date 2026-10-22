// Package router decides which provider and model serve a request, and walks
// the fallback chain with retries when calls fail.
package router

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/faytonn/modelmux/internal/config"
	"github.com/faytonn/modelmux/internal/metrics"
	"github.com/faytonn/modelmux/internal/providers"
	"github.com/faytonn/modelmux/internal/resilience"
)

var (
	// ErrNoCandidates means no configured model can serve the request.
	ErrNoCandidates = errors.New("no model can serve this request")
	// ErrAllFailed means every candidate in the fallback chain failed.
	ErrAllFailed = errors.New("all providers failed")
)

// Candidate is one (provider, model) pair the router can send a request to.
type Candidate struct {
	Provider providers.Provider
	Model    config.Model
	Timeout  time.Duration // per-attempt timeout; 0 means only the request budget applies
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
	Provider   string `json:"provider"`
	Model      string `json:"model"`
	DurationMS int64  `json:"duration_ms"`
	Error      string `json:"error,omitempty"`
}

// Result is the outcome of a routed request. On failure it still carries the
// attempts that were made.
type Result struct {
	Provider string
	Model    string
	Policy   string
	Response *providers.Response
	CostUSD  float64
	Attempts []Attempt
}

// Options configures a Router.
type Options struct {
	Routing config.Routing
	Retry   config.Retry
	Breaker config.Breaker
	Metrics *metrics.Metrics // may be nil
	Logger  *slog.Logger
}

// hintPolicies maps a request's routing_hint to a policy name.
var hintPolicies = map[string]string{"cheap": "cost", "fast": "latency", "quality": "quality"}

type Router struct {
	candidates    []Candidate
	policies      map[string]Policy
	defaultPolicy Policy
	latency       *LatencyTracker
	retry         resilience.RetryPolicy
	breakers      map[string]*resilience.Breaker // one per provider
	metrics       *metrics.Metrics
	log           *slog.Logger
}

func New(candidates []Candidate, opts Options) (*Router, error) {
	latency := NewLatencyTracker(opts.Routing.LatencyWindow)
	policies := map[string]Policy{
		"cost":    CostPolicy{},
		"latency": LatencyPolicy{Stats: latency},
		"quality": QualityPolicy{},
		"rule":    RulePolicy{Rules: opts.Routing.Rules, Base: CostPolicy{}},
	}
	def, ok := policies[opts.Routing.Policy]
	if !ok {
		return nil, fmt.Errorf("unknown routing policy %q", opts.Routing.Policy)
	}
	breakers := map[string]*resilience.Breaker{}
	for _, c := range candidates {
		name := c.Provider.Name()
		if _, ok := breakers[name]; ok {
			continue // models of the same provider share one breaker
		}
		breakers[name] = resilience.NewBreaker(opts.Breaker.FailureThreshold, opts.Breaker.OpenTimeout,
			func(from, to resilience.State) {
				opts.Metrics.SetBreakerState(name, int(to))
				opts.Logger.Warn("circuit breaker state change", "provider", name, "from", from.String(), "to", to.String())
			})
		opts.Metrics.SetBreakerState(name, int(resilience.Closed))
	}

	return &Router{
		candidates:    candidates,
		policies:      policies,
		defaultPolicy: def,
		latency:       latency,
		retry: resilience.RetryPolicy{
			MaxAttempts:   opts.Retry.MaxAttempts,
			BaseDelay:     opts.Retry.BaseDelay,
			MaxDelay:      opts.Retry.MaxDelay,
			MaxRetryAfter: opts.Retry.MaxRetryAfter,
			Classify:      providers.Classify,
		},
		breakers: breakers,
		metrics:  opts.Metrics,
		log:      opts.Logger,
	}, nil
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

// Chat walks the ranked candidates in order. Each candidate gets its own
// retries; if it still fails, the next one is tried. Any provider error moves
// on to the next candidate (a 401 or a model-specific 400 at one provider says
// nothing about another), but a cancelled or expired request stops the walk.
func (r *Router) Chat(ctx context.Context, req Request) (*Result, error) {
	policy, ranked := r.plan(req)
	if len(ranked) == 0 {
		return nil, ErrNoCandidates
	}

	res := &Result{Policy: policy.Name()}
	var lastErr error
	for i, c := range ranked {
		resp, err := r.tryCandidate(ctx, c, req, res)
		if err == nil {
			res.Provider, res.Model, res.Response = c.Provider.Name(), c.Model.Name, resp
			res.CostUSD = c.Model.Cost(resp.InputTokens, resp.OutputTokens)
			r.metrics.ObserveUsage(c.Provider.Name(), c.Model.Name, resp.InputTokens, resp.OutputTokens, res.CostUSD)
			return res, nil
		}
		lastErr = err
		if ctx.Err() != nil {
			return res, fmt.Errorf("request budget exhausted: %w", ctx.Err())
		}
		if i < len(ranked)-1 {
			r.metrics.ObserveFallback(c.Provider.Name(), c.Model.Name)
			// Skipping an open breaker happens on every request during an outage;
			// the state change itself is already logged once at WARN.
			level := slog.LevelWarn
			if errors.Is(err, resilience.ErrOpen) {
				level = slog.LevelDebug
			}
			r.log.Log(ctx, level, "candidate failed, falling back",
				"failed", c.Key(), "next", ranked[i+1].Key(), "error", err)
		}
	}
	return res, fmt.Errorf("%w: last error: %w", ErrAllFailed, lastErr)
}

// tryCandidate calls one candidate with retries, recording every attempt.
// Each attempt first asks the provider's circuit breaker for permission, so a
// breaker that opens mid-retry stops the retries too.
func (r *Router) tryCandidate(ctx context.Context, c Candidate, req Request, res *Result) (*providers.Response, error) {
	retry := r.retry
	retry.OnRetry = func(attempt int, err error, delay time.Duration) {
		r.metrics.ObserveRetry(c.Provider.Name(), c.Model.Name)
		r.log.InfoContext(ctx, "retrying", "candidate", c.Key(), "attempt", attempt, "delay", delay, "error", err)
	}
	breaker := r.breakers[c.Provider.Name()]

	return resilience.Do(ctx, retry, func(ctx context.Context) (*providers.Response, error) {
		a := Attempt{Provider: c.Provider.Name(), Model: c.Model.Name}
		if !breaker.Allow() {
			// Not retryable by providers.Classify, so the router moves straight to the next candidate.
			err := fmt.Errorf("%s: %w", c.Provider.Name(), resilience.ErrOpen)
			a.Error = err.Error()
			res.Attempts = append(res.Attempts, a)
			r.metrics.ObserveAttempt(c.Provider.Name(), c.Model.Name, metrics.OutcomeBreakerOpen, 0)
			return nil, err
		}

		attemptCtx := ctx
		if c.Timeout > 0 {
			var cancel context.CancelFunc
			attemptCtx, cancel = context.WithTimeout(ctx, c.Timeout)
			defer cancel()
		}
		start := time.Now()
		resp, err := c.Provider.Chat(attemptCtx, providers.Request{Model: c.Model.Name, Messages: req.Messages, MaxTokens: req.MaxTokens})
		elapsed := time.Since(start)
		a.DurationMS = elapsed.Milliseconds()

		retryable, _ := providers.Classify(err)
		switch {
		case err == nil:
			breaker.Success()
			r.latency.Observe(c.Provider.Name(), elapsed)
		case ctx.Err() != nil:
			breaker.Release() // the request was cancelled or ran out of time: says nothing about the provider
		case retryable:
			breaker.Failure() // 5xx, 429, timeout, network error
		default:
			breaker.Success() // e.g. 400/401: the provider is up, it rejected this request
		}
		if err != nil {
			a.Error = err.Error()
		}
		res.Attempts = append(res.Attempts, a)
		r.metrics.ObserveAttempt(c.Provider.Name(), c.Model.Name, outcome(err), elapsed)
		return resp, err
	})
}

// outcome maps an attempt's error to the metrics "outcome" label.
func outcome(err error) string {
	var pe *providers.Error
	switch {
	case err == nil:
		return metrics.OutcomeSuccess
	case errors.As(err, &pe) && pe.StatusCode == http.StatusTooManyRequests:
		return metrics.OutcomeRateLimited
	case errors.Is(err, context.DeadlineExceeded):
		return metrics.OutcomeTimeout
	case errors.Is(err, context.Canceled):
		return metrics.OutcomeCanceled
	}
	return metrics.OutcomeError
}
