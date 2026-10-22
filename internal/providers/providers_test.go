package providers

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestAnthropicChat(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/messages" {
			t.Errorf("path = %s, want /v1/messages", r.URL.Path)
		}
		if r.Header.Get("x-api-key") != "test-key" || r.Header.Get("anthropic-version") != anthropicVersion {
			t.Errorf("missing auth or version headers: %v", r.Header)
		}
		var body anthropicRequest
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if body.System != "be brief" || len(body.Messages) != 1 || body.Messages[0].Role != "user" {
			t.Errorf("system prompt not moved out of messages: %+v", body)
		}
		w.Write([]byte(`{"content":[{"type":"text","text":"hello"}],"usage":{"input_tokens":12,"output_tokens":3}}`))
	}))
	defer srv.Close()

	p := NewAnthropic("anthropic", srv.URL, "test-key", srv.Client())
	resp, err := p.Chat(context.Background(), Request{Model: "claude-x", MaxTokens: 10, Messages: []Message{
		{Role: "system", Content: "be brief"},
		{Role: "user", Content: "hi"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Content != "hello" || resp.InputTokens != 12 || resp.OutputTokens != 3 {
		t.Errorf("resp = %+v", resp)
	}
}

func TestOpenAIChat(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" || r.Header.Get("Authorization") != "Bearer test-key" {
			t.Errorf("bad request: %s %v", r.URL.Path, r.Header)
		}
		w.Write([]byte(`{"choices":[{"message":{"content":"hello"}}],"usage":{"prompt_tokens":12,"completion_tokens":3}}`))
	}))
	defer srv.Close()

	p := NewOpenAI("openai", srv.URL, "test-key", srv.Client())
	resp, err := p.Chat(context.Background(), Request{Model: "gpt-x", MaxTokens: 10, Messages: []Message{{Role: "user", Content: "hi"}}})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Content != "hello" || resp.InputTokens != 12 || resp.OutputTokens != 3 {
		t.Errorf("resp = %+v", resp)
	}
}

func TestHTTPErrorsAreClassified(t *testing.T) {
	tests := []struct {
		name          string
		status        int
		header        map[string]string
		wantRetryable bool
		wantAfter     time.Duration
	}{
		{"429 with Retry-After seconds", 429, map[string]string{"Retry-After": "2"}, true, 2 * time.Second},
		{"429 with retry-after-ms", 429, map[string]string{"Retry-After-Ms": "250"}, true, 250 * time.Millisecond},
		{"503 is retryable", 503, nil, true, 0},
		{"529 overloaded is retryable", 529, nil, true, 0},
		{"400 is not retryable", 400, nil, false, 0},
		{"401 is not retryable", 401, nil, false, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				for k, v := range tt.header {
					w.Header().Set(k, v)
				}
				w.WriteHeader(tt.status)
				w.Write([]byte(`{"error":"nope"}`))
			}))
			defer srv.Close()

			_, err := NewAnthropic("anthropic", srv.URL, "k", srv.Client()).
				Chat(context.Background(), Request{Model: "m", MaxTokens: 1, Messages: []Message{{Role: "user", Content: "hi"}}})

			var pe *Error
			if !errors.As(err, &pe) || pe.StatusCode != tt.status {
				t.Fatalf("err = %v, want *Error with status %d", err, tt.status)
			}
			retryable, after := Classify(err)
			if retryable != tt.wantRetryable || after != tt.wantAfter {
				t.Errorf("Classify = (%v, %v), want (%v, %v)", retryable, after, tt.wantRetryable, tt.wantAfter)
			}
		})
	}
}

func TestRetryAfterHTTPDate(t *testing.T) {
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	h := http.Header{"Retry-After": []string{now.Add(30 * time.Second).Format(http.TimeFormat)}}
	if got := retryAfter(h, now); got != 30*time.Second {
		t.Errorf("retryAfter = %v, want 30s", got)
	}
}

func TestClassifyContextErrors(t *testing.T) {
	if r, _ := Classify(context.Canceled); r {
		t.Error("a cancelled request must not be retried")
	}
	if r, _ := Classify(context.DeadlineExceeded); !r {
		t.Error("an attempt timeout should be retryable")
	}
}
