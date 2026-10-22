// Package server exposes the router over HTTP.
package server

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/faytonn/modelmux/internal/config"
	"github.com/faytonn/modelmux/internal/logging"
	"github.com/faytonn/modelmux/internal/metrics"
	"github.com/faytonn/modelmux/internal/providers"
	"github.com/faytonn/modelmux/internal/router"
)

const maxBodyBytes = 1 << 20 // 1 MiB

// Options configures a Server.
type Options struct {
	Router         *router.Router
	Config         config.Server
	Metrics        *metrics.Metrics // may be nil
	MetricsHandler http.Handler     // served at GET /metrics when set
	// Mocks enables PATCH /admin/mocks/{name} for these providers. Leave nil
	// in production: the endpoint has no authentication.
	Mocks  map[string]*providers.Mock
	Logger *slog.Logger
}

type Server struct {
	router  *router.Router
	cfg     config.Server
	metrics *metrics.Metrics
	mocks   map[string]*providers.Mock
	log     *slog.Logger
	mux     *http.ServeMux
}

func New(opts Options) *Server {
	s := &Server{router: opts.Router, cfg: opts.Config, metrics: opts.Metrics, mocks: opts.Mocks, log: opts.Logger, mux: http.NewServeMux()}
	// Go 1.22+ patterns can match on the HTTP method, so wrong methods get a 405 for free.
	s.mux.Handle("POST /v1/chat", s.instrument(http.HandlerFunc(s.handleChat)))
	s.mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	if opts.MetricsHandler != nil {
		s.mux.Handle("GET /metrics", opts.MetricsHandler)
	}
	if opts.Mocks != nil {
		s.mux.HandleFunc("PATCH /admin/mocks/{name}", s.handleMockUpdate)
	}
	return s
}

// Handler returns the full handler chain: request ID middleware, then routes.
func (s *Server) Handler() http.Handler { return s.withRequestID(s.mux) }

type chatRequest struct {
	Messages    []providers.Message `json:"messages"`
	MaxTokens   int                 `json:"max_tokens,omitempty"`
	RoutingHint string              `json:"routing_hint,omitempty"`
}

type usage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}

type chatResponse struct {
	RequestID        string           `json:"request_id"`
	Provider         string           `json:"provider"`
	Model            string           `json:"model"`
	Policy           string           `json:"routing_policy"`
	Content          string           `json:"content"`
	Usage            usage            `json:"usage"`
	EstimatedCostUSD float64          `json:"estimated_cost_usd"`
	LatencyMS        int64            `json:"latency_ms"`
	Attempts         []router.Attempt `json:"attempts"`
}

type errorResponse struct {
	RequestID string           `json:"request_id,omitempty"`
	Error     string           `json:"error"`
	Attempts  []router.Attempt `json:"attempts,omitempty"`
}

func (s *Server) handleChat(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	reqID := logging.RequestID(r.Context())

	var req chatRequest
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, errorResponse{RequestID: reqID, Error: "invalid JSON body: " + err.Error()})
		return
	}
	if err := validate(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, errorResponse{RequestID: reqID, Error: err.Error()})
		return
	}
	if req.MaxTokens == 0 {
		req.MaxTokens = s.cfg.DefaultMaxTokens
	}

	// r.Context() is cancelled if the client disconnects; the timeout bounds
	// the whole request, including any retries and fallbacks.
	ctx, cancel := context.WithTimeout(r.Context(), s.cfg.RequestTimeout)
	defer cancel()

	res, err := s.router.Chat(ctx, router.Request{Messages: req.Messages, MaxTokens: req.MaxTokens, Hint: req.RoutingHint})
	if err != nil {
		var attempts []router.Attempt
		if res != nil {
			attempts = res.Attempts
		}
		s.log.WarnContext(ctx, "chat request failed", "error", err, "attempts", len(attempts))
		writeJSON(w, statusFor(err), errorResponse{RequestID: reqID, Error: err.Error(), Attempts: attempts})
		return
	}

	s.log.InfoContext(ctx, "chat request served",
		"provider", res.Provider, "model", res.Model, "policy", res.Policy,
		"attempts", len(res.Attempts), "cost_usd", res.CostUSD)
	writeJSON(w, http.StatusOK, chatResponse{
		RequestID:        reqID,
		Provider:         res.Provider,
		Model:            res.Model,
		Policy:           res.Policy,
		Content:          res.Response.Content,
		Usage:            usage{InputTokens: res.Response.InputTokens, OutputTokens: res.Response.OutputTokens},
		EstimatedCostUSD: res.CostUSD,
		LatencyMS:        time.Since(start).Milliseconds(),
		Attempts:         res.Attempts,
	})
}

func validate(req *chatRequest) error {
	if len(req.Messages) == 0 {
		return errors.New("messages must not be empty")
	}
	for _, m := range req.Messages {
		switch m.Role {
		case "system", "user", "assistant":
		default:
			return errors.New(`message role must be "system", "user" or "assistant"`)
		}
	}
	if req.MaxTokens < 0 {
		return errors.New("max_tokens must be positive")
	}
	switch req.RoutingHint {
	case "", "cheap", "fast", "quality":
	default:
		return errors.New(`routing_hint must be "cheap", "fast" or "quality"`)
	}
	return nil
}

// statusClientClosedRequest is nginx's non-standard 499: the client hung up
// before we answered. Using it keeps cancelled requests from showing up as
// 502s (provider failures) in metrics.
const statusClientClosedRequest = 499

// statusFor maps router errors to HTTP status codes.
func statusFor(err error) int {
	switch {
	case errors.Is(err, context.Canceled):
		return statusClientClosedRequest
	case errors.Is(err, context.DeadlineExceeded):
		return http.StatusGatewayTimeout
	case errors.Is(err, router.ErrNoCandidates):
		return http.StatusUnprocessableEntity
	default:
		return http.StatusBadGateway
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
