package logging

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"testing"
)

func TestHandlerAddsRequestID(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(Handler{slog.NewJSONHandler(&buf, nil)}).With("component", "test")

	log.InfoContext(WithRequestID(context.Background(), "abc123"), "hello")

	var rec map[string]any
	if err := json.Unmarshal(buf.Bytes(), &rec); err != nil {
		t.Fatal(err)
	}
	if rec["request_id"] != "abc123" || rec["component"] != "test" {
		t.Errorf("record = %v, want request_id=abc123 and component=test", rec)
	}
}

func TestNewRequestIDIsUnique(t *testing.T) {
	a, b := NewRequestID(), NewRequestID()
	if len(a) != 16 || a == b {
		t.Errorf("got %q and %q, want two different 16-char IDs", a, b)
	}
}
