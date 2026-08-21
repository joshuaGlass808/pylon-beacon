package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseLogship(t *testing.T) {
	ls, err := parseLogship("app", "/var/log/app/*.log | My App")
	if err != nil {
		t.Fatal(err)
	}
	if ls.Glob != "/var/log/app/*.log" || ls.Service != "my-app" {
		t.Fatalf("parsed wrong: %+v", ls)
	}
	if _, err := parseLogship("bad", " | svc"); err == nil {
		t.Fatal("empty glob accepted")
	}
	// service is optional
	ls, err = parseLogship("k8s", "/var/log/containers/*.log")
	if err != nil || ls.Service != "" {
		t.Fatalf("optional service: %+v err=%v", ls, err)
	}
}

func TestLogshipDeriveService(t *testing.T) {
	// the Kubernetes CRI filename convention → container name
	got := logshipDeriveService("/var/log/containers/web-5d9f_prod_nginx-0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef.log")
	if got != "nginx" {
		t.Fatalf("CRI name: got %q", got)
	}
	// anything else → the file's own name
	if got := logshipDeriveService("/var/log/My App.log"); got != "my-app" {
		t.Fatalf("plain name: got %q", got)
	}
}

func TestLogshipUnwrap(t *testing.T) {
	// docker json-file
	msg, ts := logshipUnwrap(`{"log":"write failed\n","stream":"stderr","time":"2026-08-21T02:00:00.5Z"}`)
	if msg != "write failed" || ts.IsZero() {
		t.Fatalf("docker unwrap: %q %v", msg, ts)
	}
	// kubernetes CRI
	msg, ts = logshipUnwrap("2026-08-21T02:00:00.123456789Z stdout F request handled")
	if msg != "request handled" || ts.IsZero() {
		t.Fatalf("CRI unwrap: %q %v", msg, ts)
	}
	// plain lines pass through untouched, no timestamp claimed
	msg, ts = logshipUnwrap("plain old line")
	if msg != "plain old line" || !ts.IsZero() {
		t.Fatalf("plain unwrap: %q %v", msg, ts)
	}
}

func TestLogshipLevel(t *testing.T) {
	for line, want := range map[string]string{
		"ERROR: disk full":            "error",
		"level=warn slow query":       "warn",
		"panic: runtime error":        "fatal",
		"request handled in 3ms":      "info",
		`{"level":"error","msg":"x"}`: "error",
	} {
		if got := logshipLevel(line); got != want {
			t.Fatalf("level(%q)=%q want %q", line, got, want)
		}
	}
}

func TestLogshipCollect(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "app.log")
	if err := os.WriteFile(path, []byte("old line one\nold line two\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	ls, _ := parseLogship("app", path+" | app")

	// first sight primes at EOF — history must never ship
	if rows := ls.collect(); len(rows) != 0 {
		t.Fatalf("first collect shipped history: %d rows", len(rows))
	}

	// appended lines ship; a partial line without its newline waits
	f, _ := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	f.WriteString("fresh ERROR: line\npartial without newline")
	f.Close()
	rows := ls.collect()
	if len(rows) != 1 || rows[0].Msg != "fresh ERROR: line" || rows[0].Level != "error" || rows[0].Service != "app" {
		t.Fatalf("append collect: %+v", rows)
	}

	// the partial completes -> ships once, whole
	f, _ = os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	f.WriteString(" now complete\n")
	f.Close()
	rows = ls.collect()
	if len(rows) != 1 || !strings.HasSuffix(rows[0].Msg, "now complete") || strings.Count(rows[0].Msg, "partial") != 1 {
		t.Fatalf("partial completion: %+v", rows)
	}

	// truncation (rotation) restarts from the top of the new file
	if err := os.WriteFile(path, []byte("after rotate\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	rows = ls.collect()
	if len(rows) != 1 || rows[0].Msg != "after rotate" {
		t.Fatalf("rotation: %+v", rows)
	}
}
