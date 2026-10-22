// Package router decides which provider and model serve a request.
package router

import (
	"context"
	"errors"
	"log/slog"

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
	Response *providers.Response
	CostUSD  float64
	Attempts []Attempt
}

type Router struct {
	candidates []Candidate
	log        *slog.Logger
}

func New(candidates []Candidate, log *slog.Logger) *Router {
	return &Router{candidates: candidates, log: log}
}

// Chat sends the request to the first candidate. Routing policies, retries and
// fallbacks are layered on in later steps.
func (r *Router) Chat(ctx context.Context, req Request) (*Result, error) {
	if len(r.candidates) == 0 {
		return nil, ErrNoCandidates
	}
	c := r.candidates[0]
	resp, err := c.Provider.Chat(ctx, providers.Request{Model: c.Model.Name, Messages: req.Messages, MaxTokens: req.MaxTokens})
	attempt := Attempt{Provider: c.Provider.Name(), Model: c.Model.Name}
	if err != nil {
		attempt.Error = err.Error()
		return &Result{Attempts: []Attempt{attempt}}, err
	}
	return &Result{
		Provider: c.Provider.Name(),
		Model:    c.Model.Name,
		Response: resp,
		CostUSD:  c.Model.Cost(resp.InputTokens, resp.OutputTokens),
		Attempts: []Attempt{attempt},
	}, nil
}
