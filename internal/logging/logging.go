// Package logging carries a request ID through context.Context and adds it to
// every slog record logged with a *Context method (InfoContext, WarnContext...).
package logging

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"log/slog"
)

// ctxKey is unexported so no other package can read or overwrite our value.
type ctxKey struct{}

func WithRequestID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, ctxKey{}, id)
}

// RequestID returns the request ID stored in ctx, or "".
func RequestID(ctx context.Context) string {
	id, _ := ctx.Value(ctxKey{}).(string)
	return id
}

// NewRequestID returns a random 16-character hex ID.
func NewRequestID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b) // crypto/rand.Read never returns an error
	return hex.EncodeToString(b)
}

// Handler wraps another slog.Handler and adds a request_id attribute when the
// context has one. Embedding slog.Handler gives us Enabled for free; only the
// methods we change are written out.
type Handler struct {
	slog.Handler
}

func (h Handler) Handle(ctx context.Context, r slog.Record) error {
	if id := RequestID(ctx); id != "" {
		r.AddAttrs(slog.String("request_id", id))
	}
	return h.Handler.Handle(ctx, r)
}

// WithAttrs and WithGroup must return our wrapper too, or loggers derived with
// logger.With(...) would silently lose the request ID.
func (h Handler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return Handler{h.Handler.WithAttrs(attrs)}
}

func (h Handler) WithGroup(name string) slog.Handler {
	return Handler{h.Handler.WithGroup(name)}
}
