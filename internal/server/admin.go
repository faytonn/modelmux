package server

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/faytonn/modelmux/internal/providers"
)

// mockUpdate is a partial update: nil pointer fields are left unchanged, which
// lets a client send {"error_rate": 1} without resetting everything else.
type mockUpdate struct {
	ErrorRate     *float64 `json:"error_rate"`
	RateLimitRate *float64 `json:"rate_limit_rate"`
	Latency       *string  `json:"latency"` // Go duration, e.g. "250ms"
}

type mockState struct {
	ErrorRate     float64 `json:"error_rate"`
	RateLimitRate float64 `json:"rate_limit_rate"`
	Latency       string  `json:"latency"`
}

// handleMockUpdate changes a mock provider's behaviour at runtime, so a load
// test can start and end an outage. Only registered when admin is enabled.
func (s *Server) handleMockUpdate(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name") // the {name} wildcard from the route pattern
	m, ok := s.mocks[name]
	if !ok {
		writeJSON(w, http.StatusNotFound, errorResponse{Error: "no mock provider named " + name})
		return
	}

	var u mockUpdate
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&u); err != nil {
		writeJSON(w, http.StatusBadRequest, errorResponse{Error: "invalid JSON body: " + err.Error()})
		return
	}

	cfg := m.Config()
	if u.ErrorRate != nil {
		cfg.ErrorRate = *u.ErrorRate
	}
	if u.RateLimitRate != nil {
		cfg.RateLimitRate = *u.RateLimitRate
	}
	if u.Latency != nil {
		d, err := time.ParseDuration(*u.Latency)
		if err != nil || d < 0 {
			writeJSON(w, http.StatusBadRequest, errorResponse{Error: "latency must be a duration like 250ms"})
			return
		}
		cfg.Latency = d
	}
	if !validRate(cfg.ErrorRate) || !validRate(cfg.RateLimitRate) || cfg.ErrorRate+cfg.RateLimitRate > 1 {
		writeJSON(w, http.StatusBadRequest, errorResponse{Error: "rates must be in [0, 1] and sum to at most 1"})
		return
	}

	m.SetConfig(cfg)
	s.log.WarnContext(r.Context(), "mock provider reconfigured",
		"provider", name, "error_rate", cfg.ErrorRate, "rate_limit_rate", cfg.RateLimitRate, "latency", cfg.Latency)
	writeJSON(w, http.StatusOK, toMockState(cfg))
}

func validRate(f float64) bool { return f >= 0 && f <= 1 }

func toMockState(c providers.MockConfig) mockState {
	return mockState{ErrorRate: c.ErrorRate, RateLimitRate: c.RateLimitRate, Latency: c.Latency.String()}
}
