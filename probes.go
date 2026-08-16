package main

// Local probes — the beacon checks things on ITS side of the network and
// reports what it found.
//
// The pollers on the server live on the public internet. That is the point of
// off-site monitoring — and exactly why they can never reach the NAS at
// 192.168.x, the hypervisor's web UI, or anything else that rightly refuses
// to face the internet. The beacon already lives inside, already
// authenticates outward, and already pushes on a period. So it probes locally
// and ships the results with its vitals: monitoring for the private side of
// the network with nothing exposed and no inbound path added.
//
// Config:
//
//	[probes]
//	pihole  = http http://192.168.0.240/admin
//	nas     = tcp 192.168.0.50:445
//	gateway = ping 192.168.0.1
//	slowbox = http https://10.0.0.9:8443 10
//
// name = kind target [timeout-seconds]. Kinds: http (GET; up when the
// response code is below 500 — an auth prompt is a living service), tcp
// (connect), ping (the system ping tool, one echo). Timeout defaults to 5
// seconds: LAN gear answers in milliseconds, and a probe that needs longer
// is already telling you something.
//
// A probe that times out is a RESULT (down, at the timeout), not an absence.
// The distinction matters at the server: a missing entry means the agent
// could not run the probe this cycle; ok=false means it ran and the target
// did not answer.

import (
	"crypto/tls"
	"fmt"
	"net"
	"net/http"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"time"
)

const (
	// probeCap bounds a single agent's probe list. The server enforces its own
	// cap as well — this one just keeps a copy-pasted config from turning the
	// agent into a scanner.
	probeCap            = 20
	probeDefaultTimeout = 5 * time.Second
	probeMaxTimeout     = 30 * time.Second
	// probeMaxParallel is sized against probeCap and the timeout so a fully
	// dark LAN still fits in the collection phase: 20 probes / 10 wide = two
	// waves, 10s worst case at the default timeout — inside the phase budget
	// of any interval the beacon accepts.
	probeMaxParallel = 10
)

type probe struct {
	Name    string
	Kind    string // http | tcp | ping
	Target  string
	Timeout time.Duration
}

// probeResult is what rides in the push payload. Field names are the wire
// format — the server keys monitors off name, so name changes in a config are
// new monitors, not renames.
type probeResult struct {
	Name   string `json:"name"`
	Kind   string `json:"kind"`
	Target string `json:"target"`
	OK     bool   `json:"ok"`
	MS     int64  `json:"ms"`
}

func parseProbe(name, spec string) (*probe, error) {
	f := strings.Fields(spec)
	if len(f) < 2 {
		return nil, fmt.Errorf("want `http|tcp|ping target [timeout-seconds]`, got %q", spec)
	}
	kind := strings.ToLower(f[0])
	switch kind {
	case "http", "tcp", "ping":
	default:
		return nil, fmt.Errorf("unknown probe kind %q (want http, tcp or ping)", f[0])
	}
	target := f[1]
	timeout := probeDefaultTimeout
	if len(f) > 2 {
		n, err := strconv.Atoi(f[2])
		if err != nil || n < 1 {
			return nil, fmt.Errorf("bad timeout %q (want whole seconds)", f[2])
		}
		timeout = time.Duration(n) * time.Second
		if timeout > probeMaxTimeout {
			timeout = probeMaxTimeout
		}
	}
	if kind == "http" && !strings.HasPrefix(target, "http://") && !strings.HasPrefix(target, "https://") {
		target = "http://" + target
	}
	if kind == "tcp" && !strings.Contains(target, ":") {
		return nil, fmt.Errorf("tcp target %q needs a port (host:port)", target)
	}
	return &probe{Name: name, Kind: kind, Target: target, Timeout: timeout}, nil
}

// run executes one probe and always returns a result — up or down, never an
// error. Latency is wall time as the LAN would experience it.
func (p *probe) run() probeResult {
	start := time.Now()
	ok := false
	switch p.Kind {
	case "http":
		// A probe asserts liveness, not identity: LAN gear is overwhelmingly
		// self-signed, and refusing its certificate would report every healthy
		// NAS and router UI as down forever.
		client := &http.Client{
			Timeout: p.Timeout,
			Transport: &http.Transport{
				TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
			},
		}
		resp, err := client.Get(p.Target)
		if err == nil {
			resp.Body.Close()
			// Below 500 is alive: 200s obviously, 3xx obviously, and a 401/403
			// is a service healthy enough to demand credentials. 5xx is the
			// service telling us itself that it is broken.
			ok = resp.StatusCode < 500
		}
	case "tcp":
		conn, err := net.DialTimeout("tcp", p.Target, p.Timeout)
		if err == nil {
			conn.Close()
			ok = true
		}
	case "ping":
		// The system ping tool: no raw sockets, no privileges, works the same
		// under systemd and a service account. One echo; the exit code is the
		// verdict.
		var cmd *exec.Cmd
		if runtime.GOOS == "windows" {
			cmd = exec.Command("ping", "-n", "1", "-w", strconv.FormatInt(p.Timeout.Milliseconds(), 10), p.Target)
		} else {
			secs := int(p.Timeout.Seconds())
			if secs < 1 {
				secs = 1
			}
			cmd = exec.Command("ping", "-c", "1", "-W", strconv.Itoa(secs), p.Target)
		}
		timer := time.AfterFunc(p.Timeout+time.Second, func() {
			if cmd.Process != nil {
				cmd.Process.Kill()
			}
		})
		ok = cmd.Run() == nil
		timer.Stop()
	}
	// Wall time either way: a fast refusal (connection refused) reports its
	// honest few milliseconds, a timeout reports roughly the timeout.
	return probeResult{Name: p.Name, Kind: p.Kind, Target: p.Target, OK: ok, MS: time.Since(start).Milliseconds()}
}

// runProbes executes every probe concurrently and returns whatever finished
// inside the deadline — the same shape as runCustomAll, for the same reason:
// the collection phase must never outlast the interval, because a late push
// is a false page. A probe the deadline cuts off is dropped for THIS cycle
// (the agent was too loaded to know), while a probe that timed out on its own
// clock reports down (the target did not answer). Concurrency is capped so a
// dark LAN costs the slowest wave, not the sum of every timeout.
func runProbes(probes []*probe, deadline time.Duration) []probeResult {
	if len(probes) == 0 {
		return nil
	}
	ch := make(chan probeResult, len(probes))
	slots := make(chan struct{}, probeMaxParallel)
	for _, p := range probes {
		go func(p *probe) {
			slots <- struct{}{}
			defer func() { <-slots }()
			ch <- p.run()
		}(p)
	}
	out := []probeResult{}
	timeout := time.After(deadline)
	for range probes {
		select {
		case r := <-ch:
			out = append(out, r)
		case <-timeout:
			return out
		}
	}
	return out
}
