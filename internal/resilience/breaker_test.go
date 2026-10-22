package resilience

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeClock lets tests move time forward instead of sleeping.
type fakeClock struct{ t time.Time }

func (c *fakeClock) now() time.Time          { return c.t }
func (c *fakeClock) advance(d time.Duration) { c.t = c.t.Add(d) }

func newTestBreaker(transitions *[]string) (*Breaker, *fakeClock) {
	clock := &fakeClock{t: time.Unix(0, 0)}
	b := NewBreaker(3, 10*time.Second, func(from, to State) {
		*transitions = append(*transitions, from.String()+"->"+to.String())
	})
	b.now = clock.now
	return b, clock
}

func failN(b *Breaker, n int) {
	for range n {
		if b.Allow() {
			b.Failure()
		}
	}
}

func TestBreakerOpensAfterConsecutiveFailures(t *testing.T) {
	var tr []string
	b, _ := newTestBreaker(&tr)

	failN(b, 2)
	if b.State() != Closed {
		t.Fatalf("state after 2 failures = %v, want closed", b.State())
	}
	failN(b, 1)
	if b.State() != Open {
		t.Fatalf("state after 3 failures = %v, want open", b.State())
	}
	if b.Allow() {
		t.Error("open breaker allowed a call")
	}
}

func TestBreakerSuccessResetsFailureCount(t *testing.T) {
	var tr []string
	b, _ := newTestBreaker(&tr)

	failN(b, 2)
	b.Allow()
	b.Success()
	failN(b, 2)
	if b.State() != Closed {
		t.Errorf("state = %v, want closed: failures were not consecutive", b.State())
	}
}

func TestBreakerHalfOpenProbeSuccessCloses(t *testing.T) {
	var tr []string
	b, clock := newTestBreaker(&tr)
	failN(b, 3)

	clock.advance(9 * time.Second)
	if b.Allow() {
		t.Fatal("allowed a call before the open timeout passed")
	}
	clock.advance(time.Second)
	if !b.Allow() {
		t.Fatal("did not allow a probe after the open timeout")
	}
	if b.Allow() {
		t.Fatal("allowed a second call while the probe is in flight")
	}
	b.Success()

	if b.State() != Closed || !b.Allow() {
		t.Errorf("state = %v, want closed and accepting calls", b.State())
	}
	want := []string{"closed->open", "open->half-open", "half-open->closed"}
	if len(tr) != len(want) || tr[0] != want[0] || tr[1] != want[1] || tr[2] != want[2] {
		t.Errorf("transitions = %v, want %v", tr, want)
	}
}

func TestBreakerHalfOpenProbeFailureReopens(t *testing.T) {
	var tr []string
	b, clock := newTestBreaker(&tr)
	failN(b, 3)
	clock.advance(10 * time.Second)

	b.Allow()
	b.Failure()
	if b.State() != Open {
		t.Fatalf("state = %v, want open after failed probe", b.State())
	}
	clock.advance(5 * time.Second)
	if b.Allow() {
		t.Error("allowed a call: the open timeout should restart from the failed probe")
	}
}

func TestBreakerReleaseFreesProbeSlot(t *testing.T) {
	var tr []string
	b, clock := newTestBreaker(&tr)
	failN(b, 3)
	clock.advance(10 * time.Second)

	b.Allow()
	b.Release() // e.g. the client hung up mid-probe
	if b.State() != HalfOpen {
		t.Fatalf("state = %v, want half-open", b.State())
	}
	if !b.Allow() {
		t.Error("probe slot was not freed by Release")
	}
}

func TestBreakerAllowsExactlyOneConcurrentProbe(t *testing.T) {
	var tr []string
	b, clock := newTestBreaker(&tr)
	failN(b, 3)
	clock.advance(10 * time.Second)

	var allowed atomic.Int32
	var wg sync.WaitGroup
	for range 100 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if b.Allow() {
				allowed.Add(1)
			}
		}()
	}
	wg.Wait()
	if n := allowed.Load(); n != 1 {
		t.Errorf("%d goroutines got through a half-open breaker, want 1", n)
	}
}
