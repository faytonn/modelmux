package resilience

import (
	"context"
	"errors"
	"testing"
	"time"
)

var (
	errTransient = errors.New("transient")
	errPermanent = errors.New("permanent")
	errSlowDown  = errors.New("rate limited")
)

func classify(err error) (bool, time.Duration) {
	switch {
	case errors.Is(err, errTransient):
		return true, 0
	case errors.Is(err, errSlowDown):
		return true, 500 * time.Millisecond
	}
	return false, 0
}

// testPolicy records every wait instead of sleeping, so tests run instantly.
func testPolicy(waits *[]time.Duration) RetryPolicy {
	return RetryPolicy{
		MaxAttempts: 3,
		BaseDelay:   100 * time.Millisecond,
		MaxDelay:    time.Second,
		Classify:    classify,
		rand:        func() float64 { return 1 }, // always the jitter ceiling
		sleep: func(_ context.Context, d time.Duration) error {
			*waits = append(*waits, d)
			return nil
		},
	}
}

// failing returns fn that fails with errs in order, then succeeds.
func failing(calls *int, errs ...error) func(context.Context) (string, error) {
	return func(context.Context) (string, error) {
		*calls++
		if *calls <= len(errs) {
			return "", errs[*calls-1]
		}
		return "ok", nil
	}
}

func TestDoRetriesTransientErrorsThenSucceeds(t *testing.T) {
	var waits []time.Duration
	calls := 0
	got, err := Do(context.Background(), testPolicy(&waits), failing(&calls, errTransient, errTransient))
	if err != nil || got != "ok" {
		t.Fatalf("Do() = %q, %v; want ok, nil", got, err)
	}
	if calls != 3 {
		t.Errorf("calls = %d, want 3", calls)
	}
	want := []time.Duration{100 * time.Millisecond, 200 * time.Millisecond} // exponential
	if len(waits) != 2 || waits[0] != want[0] || waits[1] != want[1] {
		t.Errorf("waits = %v, want %v", waits, want)
	}
}

func TestDoStopsAtMaxAttempts(t *testing.T) {
	var waits []time.Duration
	calls := 0
	_, err := Do(context.Background(), testPolicy(&waits), failing(&calls, errTransient, errTransient, errTransient, errTransient))
	if !errors.Is(err, errTransient) {
		t.Fatalf("err = %v, want errTransient", err)
	}
	if calls != 3 {
		t.Errorf("calls = %d, want 3", calls)
	}
}

func TestDoDoesNotRetryPermanentErrors(t *testing.T) {
	var waits []time.Duration
	calls := 0
	_, err := Do(context.Background(), testPolicy(&waits), failing(&calls, errPermanent))
	if !errors.Is(err, errPermanent) || calls != 1 {
		t.Fatalf("err = %v, calls = %d; want errPermanent after 1 call", err, calls)
	}
}

func TestDoHonoursRetryAfter(t *testing.T) {
	var waits []time.Duration
	calls := 0
	_, err := Do(context.Background(), testPolicy(&waits), failing(&calls, errSlowDown))
	if err != nil {
		t.Fatal(err)
	}
	// Backoff alone would be 100ms; the server asked for 500ms.
	if len(waits) != 1 || waits[0] != 500*time.Millisecond {
		t.Errorf("waits = %v, want [500ms]", waits)
	}
}

func TestDoGivesUpWhenRetryAfterTooLong(t *testing.T) {
	var waits []time.Duration
	p := testPolicy(&waits)
	p.MaxRetryAfter = 200 * time.Millisecond
	calls := 0
	_, err := Do(context.Background(), p, failing(&calls, errSlowDown))
	if !errors.Is(err, errSlowDown) || calls != 1 || len(waits) != 0 {
		t.Fatalf("err = %v, calls = %d, waits = %v; want immediate give-up", err, calls, waits)
	}
}

func TestDoWillNotWaitPastDeadline(t *testing.T) {
	var waits []time.Duration
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	calls := 0
	_, err := Do(ctx, testPolicy(&waits), failing(&calls, errTransient))
	if !errors.Is(err, errTransient) || calls != 1 || len(waits) != 0 {
		t.Fatalf("err = %v, calls = %d, waits = %v; want give-up without waiting", err, calls, waits)
	}
}

func TestDoStopsWhenContextCancelledDuringWait(t *testing.T) {
	p := RetryPolicy{MaxAttempts: 5, BaseDelay: time.Hour, MaxDelay: time.Hour, Classify: classify,
		rand: func() float64 { return 1 }}
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(20*time.Millisecond, cancel)

	calls := 0
	_, err := Do(ctx, p, failing(&calls, errTransient, errTransient))
	if !errors.Is(err, context.Canceled) || !errors.Is(err, errTransient) {
		t.Fatalf("err = %v, want it to wrap both context.Canceled and errTransient", err)
	}
	if calls != 1 {
		t.Errorf("calls = %d, want 1", calls)
	}
}

func TestBackoffIsCappedAndJittered(t *testing.T) {
	p := RetryPolicy{BaseDelay: 100 * time.Millisecond, MaxDelay: time.Second, rand: func() float64 { return 1 }}
	tests := []struct {
		attempt int
		want    time.Duration
	}{
		{1, 100 * time.Millisecond},
		{2, 200 * time.Millisecond},
		{3, 400 * time.Millisecond},
		{4, 800 * time.Millisecond},
		{5, time.Second}, // capped
		{60, time.Second},
	}
	for _, tt := range tests {
		if got := p.Backoff(tt.attempt); got != tt.want {
			t.Errorf("Backoff(%d) = %v, want %v", tt.attempt, got, tt.want)
		}
	}

	p.rand = func() float64 { return 0.25 }
	if got := p.Backoff(3); got != 100*time.Millisecond {
		t.Errorf("jittered Backoff(3) = %v, want 100ms (a quarter of 400ms)", got)
	}
}
