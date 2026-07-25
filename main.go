// pylon-beacon — one binary, no open ports, full node vitals.
//
// Collects this machine's vitals (CPU, memory, per-mount disk, temperature,
// load, uptime — plus anything you add under [custom]) and PUSHES them to
// PylonMon over outbound HTTPS. Nothing listens, nothing is scraped: it works
// behind NAT, CGNAT, and firewalls. If the box goes silent, PylonMon pages
// you — the silence is the signal.
//
// Standard library only. Config: /etc/pylon-beacon.conf (Linux) or
// beacon.conf next to the executable (Windows). See README.md.
package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"
)

const version = "0.2.0"

type config struct {
	Key      string
	URL      string
	Node     string
	Interval int
	Custom   map[string]string // metric name -> command
	Logwatch []*logWatch       // [logwatch] entries — see logwatch.go
}

func defaultConfigPath() string {
	if runtime.GOOS == "windows" {
		exe, err := os.Executable()
		if err == nil {
			return filepath.Join(filepath.Dir(exe), "beacon.conf")
		}
		return "beacon.conf"
	}
	return "/etc/pylon-beacon.conf"
}

// loadConfig parses the ini-ish config: `key = value` lines, comments with #,
// and a [custom] section whose entries are metric-name = command.
func loadConfig(path string) (*config, error) {
	cfg := &config{URL: "https://pylonmon.com", Interval: 20, Custom: map[string]string{}}
	b, err := os.ReadFile(path)
	if err != nil {
		return cfg, err
	}
	section := ""
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, ";") {
			continue
		}
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			section = strings.ToLower(strings.Trim(line, "[]"))
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		k, v = strings.TrimSpace(k), strings.TrimSpace(v)
		if section == "custom" {
			if k != "" && v != "" {
				cfg.Custom[sanitizeMetricName(k)] = v
			}
			continue
		}
		if section == "logwatch" {
			if k == "" || v == "" {
				continue
			}
			w, werr := parseLogwatch(sanitizeMetricName(k), v)
			if werr != nil {
				fmt.Fprintln(os.Stderr, "logwatch "+k+": "+werr.Error()+" (entry skipped)")
				continue
			}
			cfg.Logwatch = append(cfg.Logwatch, w)
			continue
		}
		switch strings.ToLower(k) {
		case "key":
			cfg.Key = v
		case "url":
			cfg.URL = strings.TrimRight(v, "/")
		case "node":
			cfg.Node = v
		case "interval":
			if n, err := strconv.Atoi(v); err == nil {
				cfg.Interval = n
			}
		}
	}
	return cfg, nil
}

func sanitizeMetricName(s string) string {
	s = strings.TrimSpace(strings.ToLower(s))
	s = strings.Join(strings.Fields(s), "_")
	if len(s) > 48 {
		s = s[:48]
	}
	return s
}

var numRe = regexp.MustCompile(`-?\d+(\.\d+)?`)

// runCustom executes one [custom] command and returns the first number in its
// output. A failing or numberless command skips the metric — never the push.
func runCustom(command string) (float64, bool) {
	var cmd *exec.Cmd
	if runtime.GOOS == "windows" {
		cmd = exec.Command("cmd", "/C", command)
	} else {
		cmd = exec.Command("sh", "-c", command)
	}
	done := make(chan struct{})
	var out []byte
	var err error
	go func() { out, err = cmd.CombinedOutput(); close(done) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		if cmd.Process != nil {
			cmd.Process.Kill()
		}
		return 0, false
	}
	if err != nil && len(out) == 0 {
		return 0, false
	}
	m := numRe.Find(out)
	if m == nil {
		return 0, false
	}
	f, perr := strconv.ParseFloat(string(m), 64)
	return f, perr == nil
}

func push(cfg *config, metrics map[string]any, samples map[string]string) error {
	payload := map[string]any{
		"node": cfg.Node, "interval": cfg.Interval, "metrics": metrics,
	}
	if len(samples) > 0 {
		// text samples captured by [logwatch] — servers that predate them
		// simply ignore the field
		payload["samples"] = samples
	}
	body, _ := json.Marshal(payload)
	req, err := http.NewRequest("POST", cfg.URL+"/api/ingest/exporter", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+cfg.Key)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "pylon-beacon/"+version+" ("+runtime.GOOS+")")
	resp, err := (&http.Client{Timeout: 15 * time.Second}).Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		var e struct {
			Error string `json:"error"`
		}
		json.NewDecoder(resp.Body).Decode(&e)
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, e.Error)
	}
	return nil
}

// gather runs the platform collectors, every [custom] command, and every
// [logwatch] scan. The second return value is the logwatch text samples.
func gather(cfg *config) (map[string]any, map[string]string) {
	metrics := collect() // platform-specific (collect_linux.go / collect_windows.go)
	for name, command := range cfg.Custom {
		if v, ok := runCustom(command); ok {
			metrics[name] = v
		}
	}
	counts, samples := scanLogwatches(cfg.Logwatch)
	for name, n := range counts {
		metrics[name] = n
	}
	return metrics, samples
}

// banner prints the pylon-beacon mark once at startup when stdout is an
// interactive terminal — never into pipes, service journals, or --once runs.
// ANSI color only where the terminal advertises support; plain art otherwise.
func banner(cfg *config) {
	st, err := os.Stdout.Stat()
	if err != nil || st.Mode()&os.ModeCharDevice == 0 {
		return
	}
	colorOK := os.Getenv("TERM") != "dumb"
	if runtime.GOOS == "windows" && os.Getenv("WT_SESSION") == "" && os.Getenv("TERM") == "" && os.Getenv("ANSICON") == "" {
		colorOK = false // classic conhost has no VT processing unless something enabled it
	}
	c := func(code, s string) string {
		if !colorOK {
			return s
		}
		return "\x1b[" + code + "m" + s + "\x1b[0m"
	}
	const peri, mint, blue, core, dim = "38;5;111", "38;5;49", "38;5;81", "38;5;122", "2"
	fmt.Println()
	fmt.Println("      " + c(peri, "_.-") + c(core, "o") + c(peri, "-._"))
	fmt.Println("    " + c(peri, ".'       '.") + "        " + c(core, "pylon-beacon") + " " + c(dim, version))
	fmt.Println("      " + c(mint, ".-\"\"-."))
	fmt.Println("     " + c(mint, "/  ") + c(blue, "~~") + c(mint, "  \\") + "          " + c(dim, "node  ") + cfg.Node)
	fmt.Println("     " + c(mint, "\\      /") + "          " + c(dim, "to    ") + cfg.URL)
	fmt.Println("      " + c(mint, "`-..-'") + "           " + c(dim, "every ") + strconv.Itoa(cfg.Interval) + "s")
	fmt.Println()
}

func main() {
	cfgPath := flag.String("config", defaultConfigPath(), "path to the config file")
	once := flag.Bool("once", false, "collect and push a single time, then exit")
	showVersion := flag.Bool("version", false, "print the version and exit")
	flag.Parse()
	if *showVersion {
		fmt.Println("pylon-beacon", version)
		return
	}

	cfg, err := loadConfig(*cfgPath)
	if err != nil && !os.IsNotExist(err) {
		log.Fatalf("config %s: %v", *cfgPath, err)
	}
	// environment overrides (and the way to run with no config file at all)
	if v := os.Getenv("PYLON_KEY"); v != "" {
		cfg.Key = v
	}
	if v := os.Getenv("PYLON_URL"); v != "" {
		cfg.URL = strings.TrimRight(v, "/")
	}
	if v := os.Getenv("PYLON_NODE"); v != "" {
		cfg.Node = v
	}
	if v := os.Getenv("PYLON_INTERVAL"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			cfg.Interval = n
		}
	}
	if cfg.Key == "" {
		log.Fatalf("no API key: set `key = …` in %s or the PYLON_KEY environment variable (create an ingest-scoped key in PylonMon → Settings → Admin → Status page & API)", *cfgPath)
	}
	if cfg.Node == "" {
		if h, err := os.Hostname(); err == nil {
			cfg.Node = h
		} else {
			cfg.Node = "unnamed-node"
		}
	}
	if cfg.Interval < 15 {
		cfg.Interval = 15
	}

	if !*once {
		banner(cfg)
	}
	log.Printf("pylon-beacon %s: node %q -> %s every %ds (%d custom metric(s), %d log watch(es))",
		version, cfg.Node, cfg.URL, cfg.Interval, len(cfg.Custom), len(cfg.Logwatch))

	for {
		metrics, samples := gather(cfg)
		if err := push(cfg, metrics, samples); err != nil {
			log.Printf("push failed (will retry): %v", err)
		} else {
			log.Printf("pushed %d metric(s)", len(metrics))
		}
		if *once {
			return
		}
		time.Sleep(time.Duration(cfg.Interval) * time.Second)
	}
}
