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
	"github.com/faytonn/modelmux/internal/providers"
	"github.com/faytonn/modelmux/internal/router"
)

const maxBodyBytes = 1 << 20 // 1 MiB

type Server struct {
	router *router.Router
	cfg    config.Server
	log    *slog.Logger
	mux    *http.ServeMux
}

func New(r *router.Router, cfg config.Server, log *slog.Logger) *Server {
	s := &Server{router: r, cfg: cfg, log: log, mux: http.NewServeMux()}
	// Go 1.22+ patterns can match on the HTTP method, so wrong methods get a 405 for free.
	s.mux.HandleFunc("POST /v1/chat", s.handleChat)
	s.mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	return s
}

func (s *Server) Handler() http.Handler { return s.mux }

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
	Provider         string           `json:"provider"`
	Model            string           `json:"model"`
	Content          string           `json:"content"`
	Usage            usage            `json:"usage"`
	EstimatedCostUSD float64          `json:"estimated_cost_usd"`
	LatencyMS        int64            `json:"latency_ms"`
	Attempts         []router.Attempt `json:"attempts"`
}

type errorResponse struct {
	Error    string           `json:"error"`
	Attempts []router.Attempt `json:"attempts,omitempty"`
}

func (s *Server) handleChat(w http.ResponseWriter, r *http.Request) {
	start := time.Now()

	var req chatRequest
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, errorResponse{Error: "invalid JSON body: " + err.Error()})
		return
	}
	if err := validate(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, errorResponse{Error: err.Error()})
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
		s.log.Warn("chat request failed", "error", err)
		writeJSON(w, statusFor(err), errorResponse{Error: err.Error(), Attempts: attempts})
		return
	}

	writeJSON(w, http.StatusOK, chatResponse{
		Provider:         res.Provider,
		Model:            res.Model,
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

// statusFor maps router errors to HTTP status codes.
func statusFor(err error) int {
	switch {
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
