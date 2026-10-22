package router

import (
	"testing"
	"time"
)

func TestLatencyTrackerP95(t *testing.T) {
	lt := NewLatencyTracker(100)
	if _, ok := lt.P95("a"); ok {
		t.Fatal("P95 with no samples should report ok=false")
	}
	for i := 1; i <= 100; i++ {
		lt.Observe("a", time.Duration(i)*time.Millisecond)
	}
	if got, _ := lt.P95("a"); got != 95*time.Millisecond {
		t.Errorf("P95 = %v, want 95ms", got)
	}
}

func TestLatencyTrackerWindowAgesOutOldSamples(t *testing.T) {
	lt := NewLatencyTracker(10)
	for range 10 {
		lt.Observe("a", time.Second) // provider was slow...
	}
	for range 10 {
		lt.Observe("a", time.Millisecond) // ...and has recovered
	}
	if got, _ := lt.P95("a"); got != time.Millisecond {
		t.Errorf("P95 = %v, want 1ms once slow samples aged out", got)
	}
}
