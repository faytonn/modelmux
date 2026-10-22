// Package resilience holds generic reliability building blocks: retries with
// backoff and a circuit breaker. It knows nothing about HTTP or LLMs; callers
// decide what counts as a retryable error.
package resilience

import (
	"context"
	"fmt"
	"math/rand/v2"
	"time"
)

// RetryPolicy configures Do.
type RetryPolicy struct {
	MaxAttempts   int // total attempts, including the first
	BaseDelay     time.Duration
	MaxDelay      time.Duration
	MaxRetryAfter time.Duration // if the server asks us to wait longer than this, give up so the caller can fall back

	// Classify reports whether err is worth retrying and any delay the server
	// asked for (e.g. Retry-After). If nil, nothing is retried.
	Classify func(err error) (retryable bool, retryAfter time.Duration)
	// OnRetry, if set, is called before each wait. Used for logging and metrics.
	OnRetry func(attempt int, err error, delay time.Duration)

	// Test hooks; nil means use the real implementation.
	rand  func() float64
	sleep func(ctx context.Context, d time.Duration) error
}

// Backoff returns the wait before retrying after the given attempt (1-based),
// using "full jitter": a random duration in [0, min(MaxDelay, BaseDelay*2^(attempt-1))).
// Jitter spreads retries out so that many clients failing at the same moment
// do not all retry at the same moment too.
func (p RetryPolicy) Backoff(attempt int) time.Duration {
	ceiling := p.BaseDelay
	for i := 1; i < attempt && ceiling < p.MaxDelay; i++ {
		ceiling *= 2
	}
	ceiling = min(ceiling, p.MaxDelay)

	random := rand.Float64
	if p.rand != nil {
		random = p.rand
	}
	return time.Duration(random() * float64(ceiling))
}

// Do calls fn until it succeeds, returns a non-retryable error, runs out of
// attempts, or ctx is done. It never sleeps past ctx's deadline: if the next
// wait would not fit, it returns the last error straight away so the caller
// can spend the remaining time on a fallback instead.
func Do[T any](ctx context.Context, p RetryPolicy, fn func(ctx context.Context) (T, error)) (T, error) {
	var zero T
	for attempt := 1; ; attempt++ {
		v, err := fn(ctx)
		if err == nil {
			return v, nil
		}
		if ctx.Err() != nil || attempt >= p.MaxAttempts || p.Classify == nil {
			return zero, err
		}
		retryable, retryAfter := p.Classify(err)
		if !retryable {
			return zero, err
		}

		delay := p.Backoff(attempt)
		if retryAfter > 0 {
			if p.MaxRetryAfter > 0 && retryAfter > p.MaxRetryAfter {
				return zero, err
			}
			delay = max(delay, retryAfter) // the server's minimum wins
		}
		if deadline, ok := ctx.Deadline(); ok && time.Until(deadline) < delay {
			return zero, err
		}

		if p.OnRetry != nil {
			p.OnRetry(attempt, err, delay)
		}
		sleep := sleepCtx
		if p.sleep != nil {
			sleep = p.sleep
		}
		if sleepErr := sleep(ctx, delay); sleepErr != nil {
			return zero, fmt.Errorf("retry wait interrupted: %w (last error: %w)", sleepErr, err)
		}
	}
}

// sleepCtx waits for d or until ctx is done, whichever comes first.
func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
