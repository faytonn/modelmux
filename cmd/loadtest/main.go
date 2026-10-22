// Command loadtest drives traffic at a running gateway in three phases
// (healthy, outage, recovery) and prints once per second who served the
// requests, the error rate, p95 latency and the target's circuit breaker state.
//
// The outage is injected through the gateway's admin endpoint, so start the
// gateway with -admin:
//
//	go run ./cmd/modelmux -admin
//	go run ./cmd/loadtest
package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"maps"
	"net/http"
	"os"
	"slices"
	"strings"
	"sync"
	"time"
)

// counts is one second's worth of results. It is kept separate from the
// mutex so it can be copied out safely (copying a sync.Mutex is a bug).
type counts struct {
	ok        int
	failed    int
	fallbacks int // successful responses that needed more than one provider
	servedBy  map[string]int
	latencies []time.Duration
}

type stats struct {
	mu sync.Mutex
	c  counts
}

func newStats() *stats { return &stats{c: counts{servedBy: map[string]int{}}} }

func (s *stats) record(provider string, fellBack bool, d time.Duration, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.c.latencies = append(s.c.latencies, d)
	if err != nil {
		s.c.failed++
		return
	}
	s.c.ok++
	s.c.servedBy[provider]++
	if fellBack {
		s.c.fallbacks++
	}
}

// swap returns the current counts and resets them, so each tick reports one second.
func (s *stats) swap() counts {
	s.mu.Lock()
	defer s.mu.Unlock()
	snap := s.c
	s.c = counts{servedBy: map[string]int{}}
	return snap
}

func p95(ds []time.Duration) time.Duration {
	if len(ds) == 0 {
		return 0
	}
	slices.Sort(ds)
	return ds[(len(ds)*95+99)/100-1]
}

type chatResponse struct {
	Provider string `json:"provider"`
	Attempts []struct {
		Provider string `json:"provider"`
	} `json:"attempts"`
}

func main() {
	url := flag.String("url", "http://localhost:8080", "gateway base URL")
	concurrency := flag.Int("concurrency", 16, "concurrent clients")
	phase := flag.Duration("phase", 15*time.Second, "duration of each phase")
	target := flag.String("target", "mock-cheap", "mock provider to break during the outage phase")
	flag.Parse()

	client := &http.Client{
		Timeout:   30 * time.Second,
		Transport: &http.Transport{MaxIdleConnsPerHost: *concurrency},
	}
	if err := setErrorRate(client, *url, *target, 0); err != nil {
		fmt.Fprintln(os.Stderr, "cannot reach the admin endpoint (start the gateway with -admin):", err)
		os.Exit(1)
	}

	phases := []struct {
		name      string
		errorRate float64
	}{
		{"healthy", 0},
		{"outage", 1},
		{"recovery", 0},
	}
	fmt.Printf("load test: %d clients, %s per phase, breaking %q during the outage\n\n", *concurrency, *phase, *target)

	start := time.Now()
	for _, ph := range phases {
		if err := setErrorRate(client, *url, *target, ph.errorRate); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		fmt.Printf("--- phase %s: %s error_rate=%.0f%%\n", ph.name, *target, ph.errorRate*100)
		runPhase(client, *url, *target, ph.name, *concurrency, *phase, start)
	}

	fmt.Println("\n--- gateway counters (from /metrics)")
	for _, line := range scrape(client, *url, "modelmux_fallbacks_total", "modelmux_upstream_requests_total", "modelmux_circuit_breaker_state") {
		fmt.Println(line)
	}
}

// runPhase starts concurrency workers sending requests until the phase ends,
// printing a summary line every second.
func runPhase(client *http.Client, url, target, name string, concurrency int, d time.Duration, start time.Time) {
	ctx, cancel := context.WithTimeout(context.Background(), d)
	defer cancel()
	st := newStats()

	var wg sync.WaitGroup
	for range concurrency {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for ctx.Err() == nil {
				t0 := time.Now()
				provider, fellBack, err := send(ctx, client, url)
				if ctx.Err() != nil {
					return // phase ended mid-request; don't count it
				}
				st.record(provider, fellBack, time.Since(t0), err)
			}
		}()
	}

	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			wg.Wait()
			return
		case <-ticker.C:
			s := st.swap()
			total := s.ok + s.failed
			errPct := 0.0
			if total > 0 {
				errPct = 100 * float64(s.failed) / float64(total)
			}
			fmt.Printf("[%-8s] t=%3.0fs rps=%4d err=%5.1f%% p95=%6s fallbacks=%4d breaker(%s)=%-9s served: %s\n",
				name, time.Since(start).Seconds(), total, errPct, p95(s.latencies).Round(time.Millisecond),
				s.fallbacks, target, breakerState(client, url, target), formatServed(s.servedBy))
		}
	}
}

func send(ctx context.Context, client *http.Client, url string) (provider string, fellBack bool, err error) {
	body := `{"messages":[{"role":"user","content":"load test"}],"max_tokens":32}`
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url+"/v1/chat", strings.NewReader(body))
	if err != nil {
		return "", false, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", false, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", false, fmt.Errorf("status %d", resp.StatusCode)
	}
	var out chatResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", false, err
	}
	fellBack = len(out.Attempts) > 0 && out.Attempts[0].Provider != out.Provider
	return out.Provider, fellBack, nil
}

func setErrorRate(client *http.Client, url, mock string, rate float64) error {
	body, _ := json.Marshal(map[string]float64{"error_rate": rate})
	req, err := http.NewRequest(http.MethodPatch, url+"/admin/mocks/"+mock, bytes.NewReader(body))
	if err != nil {
		return err
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("PATCH /admin/mocks/%s: %s", mock, resp.Status)
	}
	return nil
}

var breakerNames = map[string]string{"0": "closed", "1": "open", "2": "half-open"}

func breakerState(client *http.Client, url, provider string) string {
	prefix := fmt.Sprintf(`modelmux_circuit_breaker_state{provider=%q} `, provider)
	for _, line := range scrape(client, url, prefix) {
		if name, ok := breakerNames[strings.TrimPrefix(line, prefix)]; ok {
			return name
		}
	}
	return "?"
}

// scrape returns the /metrics lines starting with any of the given prefixes.
func scrape(client *http.Client, url string, prefixes ...string) []string {
	resp, err := client.Get(url + "/metrics")
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	var out []string
	sc := bufio.NewScanner(resp.Body)
	for sc.Scan() {
		line := sc.Text()
		for _, p := range prefixes {
			if strings.HasPrefix(line, p) {
				out = append(out, line)
				break
			}
		}
	}
	return out
}

func formatServed(m map[string]int) string {
	var parts []string
	for _, k := range slices.Sorted(maps.Keys(m)) {
		parts = append(parts, fmt.Sprintf("%s=%d", k, m[k]))
	}
	return strings.Join(parts, " ")
}
