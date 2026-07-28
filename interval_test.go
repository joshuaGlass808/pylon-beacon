package main

import (
	"net/http"
	"net/http/httptest"
	"runtime"
	"sync"
	"testing"
	"time"
)

// The check-in period must be the configured interval — not the interval plus
// however long collection happened to take.
//
// This is the bug these tests exist for. The loop used to finish each pass with
// time.Sleep(interval), which makes the interval the gap BETWEEN cycles rather
// than the period. Nothing failed, and nothing looked wrong: the beacon logged
// a successful push every time and the config still said 20 seconds.
//
// What it cost, measured on a production node on 2026-07-28 — a beacon set to
// interval = 20 against a 30-second server-side silence window, 444 pushes over
// three hours: min 21s, p50 24s, p90 27s, p99 30s, max 31s, mean 24.3s. It
// never once pushed at 20s. Roughly 4 seconds of collection had quietly eaten
// 4.3 of the 10 seconds of headroom the configuration promised, so a normal
// tail spike crossed the line and a healthy node that was pushing the whole
// time was reported silent. A second node running the same build and the same
// config, but with a 0.2-second cycle, sat at a mean of 20.4s and never tripped
// — which is what made it look intermittent rather than systematic.
//
// A monitoring agent that reports late is worse than one that does not report:
// it manufactures false alarms about itself and trains people to ignore pages.

// waitFor polls until cond holds or the deadline passes.
func waitFor(d time.Duration, cond func() bool) bool {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(5 * time.Millisecond)
	}
	return cond()
}

// TestPushPeriodIgnoresCollectionTime is the regression proper: with collection
// deliberately made slow, arrivals must still land one interval apart.
//
// Under the old sleep-after-work loop the gaps would be interval + 400ms; the
// assertion below is written so that behaviour fails it.
func TestPushPeriodIgnoresCollectionTime(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses a POSIX shell for the slow custom command")
	}
	var mu sync.Mutex
	var arrivals []time.Time
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		arrivals = append(arrivals, time.Now())
		mu.Unlock()
		w.Header().Set("content-type", "application/json")
		w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	const period = 600 * time.Millisecond
	cfg := &config{
		Key: "test-key", URL: srv.URL, Node: "t", Interval: 1,
		// ~400ms of collection — two thirds of the period, so a sleep-after-work
		// loop lands ~1000ms apart and a ticker lands ~600ms apart.
		Custom: map[string]string{"slow": "sleep 0.4; echo 1"},
	}

	stop := make(chan struct{})
	tick := time.NewTicker(period)
	defer tick.Stop()
	done := make(chan struct{})
	go func() { defer close(done); pushLoop(cfg, period, tick.C, false, stop) }()

	count := func() int { mu.Lock(); defer mu.Unlock(); return len(arrivals) }
	if !waitFor(6*time.Second, func() bool { return count() >= 4 }) {
		close(stop)
		<-done
		t.Fatalf("only %d pushes arrived — the loop is not running", count())
	}
	close(stop)
	<-done

	mu.Lock()
	got := append([]time.Time(nil), arrivals...)
	mu.Unlock()

	// Compare against the period, not against wall-clock perfection: CI is a
	// shared machine and a scheduler hiccup is not this bug. The old behaviour
	// was period + 400ms, so a 250ms allowance separates them decisively while
	// staying well clear of ordinary jitter.
	const slack = 250 * time.Millisecond
	for i := 1; i < len(got); i++ {
		gap := got[i].Sub(got[i-1])
		if gap > period+slack {
			t.Errorf("gap %d was %v, want ~%v — collection time is being ADDED to the period "+
				"instead of coming out of it (this is the sleep-after-work bug)", i, gap, period)
		}
	}
}

// TestCustomCommandsRunConcurrently. Serially, N commands cost the SUM of their
// runtimes, and each one is allowed 10 seconds — so the collection phase was
// bounded only by 10s x N. Seven commands, as our own production config has,
// bounds it at 70 seconds: longer than any interval anyone would set.
func TestCustomCommandsRunConcurrently(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses a POSIX shell")
	}
	custom := map[string]string{}
	for _, n := range []string{"a", "b", "c", "d"} {
		custom[n] = "sleep 0.3; echo 7"
	}
	start := time.Now()
	out := runCustomAll(custom, 5*time.Second)
	elapsed := time.Since(start)

	if len(out) != 4 {
		t.Fatalf("got %d metrics, want 4: %v", len(out), out)
	}
	for n, v := range out {
		if v != 7 {
			t.Errorf("%s = %v, want 7", n, v)
		}
	}
	// Serial would be ~1.2s. Concurrency is capped at 4, so all four overlap.
	if elapsed > 900*time.Millisecond {
		t.Errorf("four 300ms commands took %v — they are still running serially", elapsed)
	}
}

// TestCustomCommandDeadlineDropsStragglers. One wedged command must cost a gap
// in a graph, never a late check-in: the push is the thing being timed by the
// server, and a metric is not worth a false page.
func TestCustomCommandDeadlineDropsStragglers(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses a POSIX shell")
	}
	custom := map[string]string{
		"fast":  "echo 1",
		"wedge": "sleep 8; echo 2",
	}
	start := time.Now()
	out := runCustomAll(custom, 400*time.Millisecond)
	elapsed := time.Since(start)

	if elapsed > 900*time.Millisecond {
		t.Errorf("runCustomAll took %v — it waited for the wedged command instead of "+
			"honouring its deadline", elapsed)
	}
	if _, ok := out["fast"]; !ok {
		t.Error("the fast metric was dropped; only the straggler should be")
	}
	if _, ok := out["wedge"]; ok {
		t.Error("the wedged command was included — it had not finished")
	}
}

// TestNoCustomCommandsIsCheap guards the common case: a beacon with no [custom]
// section must not pay anything for this machinery.
func TestNoCustomCommandsIsCheap(t *testing.T) {
	start := time.Now()
	out := runCustomAll(nil, time.Second)
	if len(out) != 0 {
		t.Fatalf("want no metrics, got %v", out)
	}
	if d := time.Since(start); d > 50*time.Millisecond {
		t.Errorf("empty custom set took %v", d)
	}
}
