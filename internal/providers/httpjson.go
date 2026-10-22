package providers

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// postJSON sends body as JSON and decodes a 2xx response into out. A non-2xx
// response becomes *Error, with Retryable and RetryAfter filled in from the
// status code and headers. Network and context errors are returned as-is;
// Classify knows how to treat them.
func postJSON(ctx context.Context, client *http.Client, provider, url string, headers map[string]string, body, out any) error {
	payload, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("%s: encode request: %w", provider, err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return fmt.Errorf("%s: build request: %w", provider, err)
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("%s: %w", provider, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return &Error{
			Provider:   provider,
			StatusCode: resp.StatusCode,
			Retryable:  IsRetryableStatus(resp.StatusCode),
			RetryAfter: retryAfter(resp.Header, time.Now()),
			Message:    strings.TrimSpace(string(msg)),
		}
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("%s: decode response: %w", provider, err)
	}
	return nil
}

// retryAfter reads how long the server asked us to wait. It understands
// OpenAI's retry-after-ms and the standard Retry-After header, which is either
// a number of seconds or an HTTP date. It returns 0 if there is no usable value.
func retryAfter(h http.Header, now time.Time) time.Duration {
	if ms, err := strconv.Atoi(h.Get("Retry-After-Ms")); err == nil && ms > 0 {
		return time.Duration(ms) * time.Millisecond
	}
	v := h.Get("Retry-After")
	if v == "" {
		return 0
	}
	if secs, err := strconv.Atoi(v); err == nil && secs > 0 {
		return time.Duration(secs) * time.Second
	}
	if t, err := http.ParseTime(v); err == nil && t.After(now) {
		return t.Sub(now)
	}
	return 0
}
