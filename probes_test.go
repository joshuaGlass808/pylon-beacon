package main

import (
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

func TestParseProbe(t *testing.T) {
	for _, tc := range []struct {
		spec    string
		wantErr string
		kind    string
		target  string
		timeout time.Duration
	}{
		{spec: "http http://192.168.0.240/admin", kind: "http", target: "http://192.168.0.240/admin", timeout: probeDefaultTimeout},
		{spec: "tcp 192.168.0.50:445", kind: "tcp", target: "192.168.0.50:445", timeout: probeDefaultTimeout},
		{spec: "ping 192.168.0.1", kind: "ping", target: "192.168.0.1", timeout: probeDefaultTimeout},
		{spec: "http https://10.0.0.9:8443 10", kind: "http", target: "https://10.0.0.9:8443", timeout: 10 * time.Second},
		// a schemeless http target gets a scheme rather than an error — configs
		// are typed by hand
		{spec: "http 192.168.0.240", kind: "http", target: "http://192.168.0.240", timeout: probeDefaultTimeout},
		// a huge timeout is capped, not obeyed
		{spec: "tcp 10.0.0.1:22 9999", kind: "tcp", target: "10.0.0.1:22", timeout: probeMaxTimeout},
		{spec: "gopher 10.0.0.1", wantErr: "unknown probe kind"},
		{spec: "http", wantErr: "want"},
		{spec: "tcp 10.0.0.1", wantErr: "needs a port"},
		{spec: "tcp 10.0.0.1:22 soon", wantErr: "bad timeout"},
	} {
		p, err := parseProbe("x", tc.spec)
		if tc.wantErr != "" {
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("parseProbe(%q) err = %v, want %q", tc.spec, err, tc.wantErr)
			}
			continue
		}
		if err != nil {
			t.Errorf("parseProbe(%q): %v", tc.spec, err)
			continue
		}
		if p.Kind != tc.kind || p.Target != tc.target || p.Timeout != tc.timeout {
			t.Errorf("parseProbe(%q) = %+v, want kind=%s target=%s timeout=%s", tc.spec, p, tc.kind, tc.target, tc.timeout)
		}
	}
}

func TestHTTPProbeVerdicts(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer up.Close()
	auth := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(401) }))
	defer auth.Close()
	broken := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(500) }))
	defer broken.Close()

	for _, tc := range []struct {
		url  string
		want bool
	}{
		{up.URL, true},
		// 401 is a service healthy enough to demand credentials — the normal
		// state of every NAS and router UI on a LAN
		{auth.URL, true},
		// 5xx is the service itself saying it is broken
		{broken.URL, false},
	} {
		p := &probe{Name: "t", Kind: "http", Target: tc.url, Timeout: 2 * time.Second}
		if got := p.run(); got.OK != tc.want {
			t.Errorf("http probe of %s: ok=%v, want %v", tc.url, got.OK, tc.want)
		}
	}
}

func TestTCPProbeUpAndDown(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()

	p := &probe{Name: "t", Kind: "tcp", Target: ln.Addr().String(), Timeout: 2 * time.Second}
	if got := p.run(); !got.OK {
		t.Errorf("tcp probe of a listening port reported down")
	}

	// a port nothing listens on: down, and quickly — a refusal is an answer
	dead := &probe{Name: "t", Kind: "tcp", Target: "127.0.0.1:1", Timeout: 2 * time.Second}
	got := dead.run()
	if got.OK {
		t.Error("tcp probe of a closed port reported up")
	}
}

// A probe that times out must come back as a RESULT (down, ~timeout), not
// vanish — the server tells "target didn't answer" apart from "agent couldn't
// ask" by exactly this.
func TestSlowTargetIsDownNotAbsent(t *testing.T) {
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(3 * time.Second)
	}))
	defer slow.Close()

	p := &probe{Name: "t", Kind: "http", Target: slow.URL, Timeout: 1 * time.Second}
	got := p.run()
	if got.OK {
		t.Fatal("a target slower than the probe timeout reported up")
	}
	if got.MS < 900 {
		t.Errorf("timeout verdict came back in %dms — suspiciously before the timeout", got.MS)
	}
}

// The collection phase must never outlast its deadline no matter how many
// probes are hung — a late push is a false page (same rule as runCustomAll).
func TestRunProbesRespectsTheDeadline(t *testing.T) {
	stuck := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(10 * time.Second)
	}))
	defer stuck.Close()

	probes := []*probe{}
	for i := 0; i < 12; i++ {
		probes = append(probes, &probe{Name: "p", Kind: "http", Target: stuck.URL, Timeout: 8 * time.Second})
	}
	start := time.Now()
	runProbes(probes, 1500*time.Millisecond)
	if e := time.Since(start); e > 3*time.Second {
		t.Fatalf("runProbes held the cycle for %s against a 1.5s deadline", e)
	}
}

func TestProbesRideTheConfig(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/beacon.conf"
	conf := "key = k\n[probes]\npihole = http http://192.168.0.240/admin\nnas = tcp 192.168.0.50:445\nbad entry = gopher nowhere\n"
	if err := writeFile(path, conf); err != nil {
		t.Fatal(err)
	}
	cfg, err := loadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Probes) != 2 {
		t.Fatalf("parsed %d probes, want 2 (the bad entry skipped, not fatal)", len(cfg.Probes))
	}
	if cfg.Probes[0].Name != "pihole" || cfg.Probes[1].Kind != "tcp" {
		t.Errorf("probes parsed wrong: %+v %+v", cfg.Probes[0], cfg.Probes[1])
	}
}

func writeFile(path, content string) error {
	return os.WriteFile(path, []byte(content), 0644)
}
