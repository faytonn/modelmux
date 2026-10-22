package resilience

import (
	"errors"
	"sync"
	"time"
)

// ErrOpen is returned (wrapped) when a call is rejected by an open breaker.
var ErrOpen = errors.New("circuit breaker open")

type State int

const (
	Closed   State = iota // normal operation
	Open                  // failing: reject calls until the open timeout passes
	HalfOpen              // testing recovery: allow a single probe call
)

func (s State) String() string {
	switch s {
	case Closed:
		return "closed"
	case Open:
		return "open"
	case HalfOpen:
		return "half-open"
	}
	return "unknown"
}

// Breaker is a consecutive-failure circuit breaker, safe for concurrent use.
//
// Closed -> Open after Threshold consecutive failures.
// Open -> HalfOpen once OpenTimeout has passed; one probe call is allowed.
// HalfOpen -> Closed if the probe succeeds, back to Open if it fails.
type Breaker struct {
	threshold   int
	openTimeout time.Duration
	onChange    func(from, to State)
	now         func() time.Time // test hook

	mu       sync.Mutex
	state    State
	failures int
	openedAt time.Time
	probing  bool // a half-open probe is in flight
}

// NewBreaker creates a closed breaker. onChange, if not nil, is called after
// every state transition, outside the breaker's lock.
func NewBreaker(threshold int, openTimeout time.Duration, onChange func(from, to State)) *Breaker {
	return &Breaker{threshold: threshold, openTimeout: openTimeout, onChange: onChange, now: time.Now}
}

func (b *Breaker) State() State {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.state
}

// Allow reports whether a call may proceed. Every allowed call must be
// followed by exactly one of Success, Failure or Release.
func (b *Breaker) Allow() bool {
	var allowed bool
	b.update(func() {
		switch b.state {
		case Closed:
			allowed = true
		case Open:
			if b.now().Sub(b.openedAt) >= b.openTimeout {
				b.state = HalfOpen
				b.probing = true
				allowed = true
			}
		case HalfOpen:
			if !b.probing {
				b.probing = true
				allowed = true
			}
		}
	})
	return allowed
}

// Success records a call that shows the provider is healthy.
func (b *Breaker) Success() {
	b.update(func() {
		switch b.state {
		case Closed:
			b.failures = 0
		case HalfOpen:
			b.state, b.failures, b.probing = Closed, 0, false
		}
	})
}

// Failure records a call that shows the provider is unhealthy.
func (b *Breaker) Failure() {
	b.update(func() {
		switch b.state {
		case Closed:
			b.failures++
			if b.failures >= b.threshold {
				b.state, b.openedAt = Open, b.now()
			}
		case HalfOpen:
			b.state, b.openedAt, b.probing = Open, b.now(), false
		}
		// Open: a slow call that started before the breaker opened; nothing to do.
	})
}

// Release ends an allowed call that says nothing about provider health (for
// example, the client cancelled). It frees the half-open probe slot.
func (b *Breaker) Release() {
	b.update(func() {
		if b.state == HalfOpen {
			b.probing = false
		}
	})
}

// update runs fn under the lock and reports any state change afterwards, so
// the callback can never deadlock by calling back into the breaker.
func (b *Breaker) update(fn func()) {
	b.mu.Lock()
	from := b.state
	fn()
	to := b.state
	b.mu.Unlock()
	if from != to && b.onChange != nil {
		b.onChange(from, to)
	}
}
