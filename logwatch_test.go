package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func mustWatch(t *testing.T, name, spec string) *logWatch {
	t.Helper()
	w, err := parseLogwatch(name, spec)
	if err != nil {
		t.Fatalf("parseLogwatch(%q): %v", spec, err)
	}
	return w
}

func TestParseLogwatch(t *testing.T) {
	w := mustWatch(t, "tb", `/var/log/app.log | Traceback \(most recent call last\): | 300`)
	if w.File != "/var/log/app.log" || w.Window != 300 {
		t.Fatalf("parsed %+v", w)
	}
	// window optional
	w = mustWatch(t, "tb", `/var/log/app.log | ERROR`)
	if w.Window != logwatchDefaultWindow {
		t.Fatalf("default window: got %d", w.Window)
	}
	// the regex may contain | — a non-numeric tail belongs to the pattern
	w = mustWatch(t, "tb", `/var/log/app.log | ERROR|FATAL`)
	if !w.Re.MatchString("FATAL: disk") {
		t.Fatal("alternation lost")
	}
	// … and a numeric tail is the window even with alternation present
	w = mustWatch(t, "tb", `/var/log/app.log | ERROR|FATAL | 60`)
	if w.Window != 60 || !w.Re.MatchString("FATAL: disk") {
		t.Fatalf("alternation+window: %+v", w)
	}
	if _, err := parseLogwatch("bad", "just-a-file"); err == nil {
		t.Fatal("want error for missing regex")
	}
	if _, err := parseLogwatch("bad", `/f | (unclosed`); err == nil {
		t.Fatal("want error for bad regex")
	}
}

func TestScanCountsAndSample(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "app.log")
	if err := os.WriteFile(path, []byte("old line before the beacon started\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	w := mustWatch(t, "tb", path+` | Traceback \(most recent call last\): | 300`)

	// first scan primes at EOF — history must not count
	w.scan(1000)
	if len(w.hits) != 0 {
		t.Fatalf("history counted: %d", len(w.hits))
	}

	tb := "Traceback (most recent call last):\n" +
		"  File \"app.py\", line 10, in handler\n" +
		"    resp = fetch(url)\n" +
		"ConnectionError: [Errno 111] refused\n"
	f, _ := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	f.WriteString("normal request line\n" + tb + "another normal line\n" + tb)
	f.Close()

	w.scan(1010)
	if got := len(w.hits); got != 2 {
		t.Fatalf("matches: got %d want 2", got)
	}
	if !strings.Contains(w.sample, "ConnectionError") || !strings.HasPrefix(w.sample, "Traceback") {
		t.Fatalf("sample block wrong:\n%s", w.sample)
	}
	if strings.Contains(w.sample, "normal") {
		t.Fatalf("sample leaked unrelated lines:\n%s", w.sample)
	}

	// window pruning: same hits, far future → count decays to zero
	w.scan(1010 + 301)
	if got := len(w.hits); got != 0 {
		t.Fatalf("window prune: got %d want 0", got)
	}
}

func TestScanRotation(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "app.log")
	os.WriteFile(path, []byte("aaaa\naaaa\naaaa\n"), 0o644)
	w := mustWatch(t, "err", path+` | ERROR | 300`)
	w.scan(1000) // prime at EOF

	// rotate: replaced by a smaller file containing a match
	os.WriteFile(path, []byte("ERROR boom\n"), 0o644)
	w.scan(1010)
	if got := len(w.hits); got != 1 {
		t.Fatalf("post-rotation match: got %d want 1", got)
	}
}

func TestBackToBackBlocksBothCount(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "app.log")
	os.WriteFile(path, []byte(""), 0o644)
	w := mustWatch(t, "tb", path+` | ^Traceback | 300`)
	w.scan(1000)
	os.WriteFile(path, []byte("Traceback\n  File a\nTraceback\n  File b\nBoom: x\n"), 0o644)
	// same size trick won't fire rotation (old size 0 < new) — plain append case
	w.scan(1010)
	if got := len(w.hits); got != 2 {
		t.Fatalf("back-to-back: got %d want 2", got)
	}
	if !strings.Contains(w.sample, "File b") {
		t.Fatalf("sample should be the LATEST block:\n%s", w.sample)
	}
}

func TestLogwatchGlob(t *testing.T) {
	dir := t.TempDir()
	a := filepath.Join(dir, "a.log")
	b := filepath.Join(dir, "b.log")
	os.WriteFile(a, []byte("old ERROR ignored\n"), 0o644)
	w, err := parseLogwatch("errs_5m", filepath.Join(dir, "*.log")+" | ERROR | 300")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().Unix()
	w.scan(now) // primes a.log at EOF
	if len(w.hits) != 0 {
		t.Fatalf("history counted: %d", len(w.hits))
	}
	// growth in an existing file and a brand-new file appearing in the glob
	f, _ := os.OpenFile(a, os.O_APPEND|os.O_WRONLY, 0o644)
	f.WriteString("fresh ERROR one\n")
	f.Close()
	os.WriteFile(b, []byte("ERROR that predates b's priming\n"), 0o644)
	w.scan(now + 1) // a counts its append; b only primes
	if len(w.hits) != 1 {
		t.Fatalf("after append: %d hits, want 1", len(w.hits))
	}
	f, _ = os.OpenFile(b, os.O_APPEND|os.O_WRONLY, 0o644)
	f.WriteString("ERROR in b\nERROR again in b\n")
	f.Close()
	w.scan(now + 2)
	if len(w.hits) != 3 {
		t.Fatalf("across files: %d hits, want 3", len(w.hits))
	}
	// a file leaving the glob is forgotten
	os.Remove(b)
	w.scan(now + 3)
	if _, ok := w.files[b]; ok {
		t.Fatal("removed file still tracked")
	}
}
