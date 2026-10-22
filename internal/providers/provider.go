// Package providers defines the interface every LLM backend implements and
// the adapters themselves (mock, Anthropic, OpenAI).
package providers

import (
	"context"
	"fmt"
	"time"
)

// Message is one turn of a conversation. Role is "system", "user" or "assistant".
type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// Request is the provider-neutral input to a chat call.
type Request struct {
	Model     string
	Messages  []Message
	MaxTokens int
}

// Response is the provider-neutral output of a chat call.
type Response struct {
	Content      string
	InputTokens  int
	OutputTokens int
}

// Provider is implemented by every backend. Implementations must stop work
// when ctx is cancelled and should return *Error for upstream failures so the
// caller can decide whether a retry makes sense.
type Provider interface {
	Name() string
	Chat(ctx context.Context, req Request) (*Response, error)
}

// Error describes a failed upstream call.
type Error struct {
	Provider   string
	StatusCode int // 0 when there was no HTTP response
	Retryable  bool
	RetryAfter time.Duration // parsed from a Retry-After header, 0 if absent
	Message    string
}

func (e *Error) Error() string {
	return fmt.Sprintf("%s: status %d: %s", e.Provider, e.StatusCode, e.Message)
}

// EstimateTokens gives a rough token count (about 4 characters per token plus
// a small per-message overhead). It is only used for routing decisions and
// cost estimates, so being approximate is fine and avoids a tokenizer dependency.
func EstimateTokens(msgs []Message) int {
	n := 0
	for _, m := range msgs {
		n += 4 + (len(m.Content)+3)/4
	}
	return n
}
