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

// TestPushPeriodSurvivesVARYINGCollectionTime is the second half of the same
// bug, and the reason gathering moved to before the wait.
//
// A ticker alone fixes the MEAN but not the SPREAD: with gather-then-push,
// consecutive check-ins land period + (this cycle's cost - the previous
// cycle's cost) apart, so a node whose collection cost varies still swings
// either side of its interval. Measured in production after the loop became a
// ticker — 57 check-ins, mean 20.1s (correct) but max 28s against a 30s
// window. The average was right and the customer would still have been paged.
//
// Here collection alternates fast/slow on purpose. Gaps must stay near the
// period regardless.
func TestPushPeriodSurvivesVaryingCollectionTime(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses a POSIX shell for the alternating custom command")
	}
	var mu sync.Mutex
	var arrivals []time.Time
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		arrivals = append(arrivals, time.Now())
		mu.Unlock()
		w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	const period = 700 * time.Millisecond
	counter := t.TempDir() + "/n"
	cfg := &config{
		Key: "test-key", URL: srv.URL, Node: "t", Interval: 1,
		// Every other cycle costs ~450ms, the rest are instant. Under
		// gather-then-push that alternation shows up directly in the gaps as
		// period +/- 450ms; with the push anchored to the tick it cannot.
		Custom: map[string]string{
			"alternating": "n=$(cat " + counter + " 2>/dev/null || echo 0); " +
				"echo $((n+1)) > " + counter + "; " +
				"[ $((n%2)) -eq 0 ] && sleep 0.45; echo 1",
		},
	}

	stop := make(chan struct{})
	tick := time.NewTicker(period)
	defer tick.Stop()
	done := make(chan struct{})
	go func() { defer close(done); pushLoop(cfg, period, tick.C, false, stop) }()

	count := func() int { mu.Lock(); defer mu.Unlock(); return len(arrivals) }
	if !waitFor(8*time.Second, func() bool { return count() >= 5 }) {
		close(stop)
		<-done
		t.Fatalf("only %d pushes arrived", count())
	}
	close(stop)
	<-done

	mu.Lock()
	got := append([]time.Time(nil), arrivals...)
	mu.Unlock()

	// Skip the first gap: the opening check-in is deliberately immediate, so
	// that interval is short by design (see pushLoop).
	const slack = 200 * time.Millisecond
	for i := 2; i < len(got); i++ {
		gap := got[i].Sub(got[i-1])
		if gap < period-slack || gap > period+slack {
			t.Errorf("gap %d was %v, want %v +/- %v — collection cost is still leaking "+
				"into the cadence", i, gap, period, slack)
		}
	}
}

// A failed push must not cost a whole interval.
//
// The loop used to log the failure and wait for the next tick, so one blip
// became a period-plus gap in the server's view — on a 20s beacon against a
// 30s silence window, a page. Over seven days across three nodes there were 17
// such failures, and one of them paged at 13:00 on 2026-07-28. In that case the
// server had actually RECEIVED the check-in and was merely too slow to answer,
// so a delivery that landed was discarded and the node reported silent.
func TestFailedPushRetriesWithinTheInterval(t *testing.T) {
	var mu sync.Mutex
	var arrivals []time.Time
	fail := true
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		first := fail
		fail = false
		if !first {
			arrivals = append(arrivals, time.Now())
		}
		mu.Unlock()
		if first {
			// the shape of the real failure: received, but no usable answer
			w.WriteHeader(502)
			return
		}
		w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	const period = 4 * time.Second // budget 2s, so 1s per attempt
	cfg := &config{Key: "k", URL: srv.URL, Node: "t", Interval: 4}
	stop := make(chan struct{})
	tick := time.NewTicker(period)
	defer tick.Stop()
	start := time.Now()
	done := make(chan struct{})
	go func() { defer close(done); pushLoop(cfg, period, tick.C, false, stop) }()
	defer func() { close(stop); <-done }()

	ok := waitFor(period-500*time.Millisecond, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(arrivals) > 0
	})
	if !ok {
		t.Fatalf("no successful check-in within one interval — the retry is waiting for the next tick, "+
			"which is exactly the bug (elapsed %v)", time.Since(start))
	}
	mu.Lock()
	at := arrivals[0].Sub(start)
	mu.Unlock()
	if at > period/2 {
		t.Errorf("the retry landed after %v; it should follow the failure immediately, well inside the %v period", at, period)
	}
}

// TestPushBudgetNeverEatsTheWholeInterval. Delivery gets half the period at
// most, so a stalled server cannot consume the slot that collection and the
// next tick need. A check-in still trying to be delivered when its successor
// is due has already missed.
func TestPushBudgetNeverEatsTheWholeInterval(t *testing.T) {
	for _, period := range []time.Duration{
		15 * time.Second, 20 * time.Second, 60 * time.Second, 5 * time.Minute,
	} {
		b := pushBudget(period)
		if b > period/2 {
			t.Errorf("period %v: budget %v exceeds half the interval", period, b)
		}
		if per := b / pushAttempts; per <= 0 {
			t.Errorf("period %v: per-attempt timeout collapsed to %v", period, per)
		}
	}
	// A normal push is ~200ms. The per-attempt deadline must stay far above
	// that at every sane interval, or a merely slow server reads as a failure.
	if per := pushBudget(20*time.Second) / pushAttempts; per < 2*time.Second {
		t.Errorf("per-attempt timeout at a 20s interval is %v — too tight against ~200ms normal latency", per)
	}
}

// TestFirstPushIsImmediate. Gathering before the wait must not delay the
// opening check-in by a whole period: a restarting agent has usually already
// been quiet for part of its window, and spending another full interval before
// speaking would page exactly the customer this is meant to protect.
func TestFirstPushIsImmediate(t *testing.T) {
	got := make(chan time.Time, 4)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case got <- time.Now():
		default:
		}
		w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	const period = 3 * time.Second
	cfg := &config{Key: "k", URL: srv.URL, Node: "t", Interval: 3}
	stop := make(chan struct{})
	tick := time.NewTicker(period)
	defer tick.Stop()
	start := time.Now()
	done := make(chan struct{})
	go func() { defer close(done); pushLoop(cfg, period, tick.C, false, stop) }()
	defer func() { close(stop); <-done }()

	select {
	case at := <-got:
		if d := at.Sub(start); d > period/2 {
			t.Errorf("first check-in took %v — it is waiting for a tick instead of "+
				"announcing immediately", d)
		}
	case <-time.After(period):
		t.Fatal("no check-in within one full period — the agent is silent on start")
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
