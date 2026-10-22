package server

import (
	"net/http"
	"time"

	"github.com/faytonn/modelmux/internal/logging"
)

// withRequestID gives every request an ID: the caller's X-Request-ID if it is
// well-formed, otherwise a new random one. The ID goes into the context (so
// every log line carries it) and back to the caller as a response header.
func (s *Server) withRequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get("X-Request-ID")
		if !validRequestID(id) {
			id = logging.NewRequestID()
		}
		w.Header().Set("X-Request-ID", id)
		next.ServeHTTP(w, r.WithContext(logging.WithRequestID(r.Context(), id)))
	})
}

// validRequestID accepts short IDs of safe characters only, so a client cannot
// inject newlines or huge values into our logs.
func validRequestID(id string) bool {
	if id == "" || len(id) > 64 {
		return false
	}
	for _, c := range id {
		ok := c == '-' || c == '_' || c == '.' ||
			(c >= '0' && c <= '9') || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
		if !ok {
			return false
		}
	}
	return true
}

// instrument records HTTP metrics and an access log line for a route.
func (s *Server) instrument(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)
		elapsed := time.Since(start)

		s.metrics.ObserveHTTP(rec.status, elapsed)
		s.log.InfoContext(r.Context(), "http request",
			"method", r.Method, "path", r.URL.Path, "status", rec.status, "duration_ms", elapsed.Milliseconds())
	})
}

// statusRecorder remembers the status code a handler wrote. Embedding
// http.ResponseWriter means it still satisfies that interface; only
// WriteHeader is overridden.
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}
