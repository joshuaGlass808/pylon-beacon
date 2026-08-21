// [logs] — ship log lines to PylonMon's log store, where they show up in the
// portal's Log streams search and under incidents ("the lines from around the
// moment it broke, on the same screen that paged you").
//
// [logwatch] answers "how MANY errors" (a metric you can alert on, with a
// sample). [logs] keeps the whole stream searchable. They share a philosophy:
// reading is incremental from a remembered offset, a fresh file starts at the
// END (installing the beacon must never replay last week), and truncation or
// rotation restarts from the top of the new file.
//
// Entries are `name = <file-or-glob> | <service>`; the service is optional.
// Globs are re-expanded every cycle, so `/var/log/containers/*.log` follows
// pods as they come and go. Two container formats are unwrapped in place:
//
//	docker json-file:  {"log":"the line\n","stream":"stderr","time":"…"}
//	kubernetes CRI:    2026-08-21T02:41:00.123Z stdout F the line
//
// On Kubernetes the service name is derived from the CRI filename convention
// (pod_namespace_container-id.log → container) unless the entry names one.
//
// Delivery is lossy by contract: lines go out once per beacon interval in one
// bounded batch, and a server that answers 429/503 (cap reached, shedding)
// costs this cycle's batch — the server protects paging above log intake, and
// the beacon respects that instead of hammering.

package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

const (
	logshipMaxScan   = 4 << 20 // bytes read per file per cycle — survives floods
	logshipLineMax   = 8 << 10 // longer lines are truncated, marked
	logshipBatchMax  = 500 << 10
	logshipBatchRows = 2000
)

var logshipClient = &http.Client{Timeout: 10 * time.Second}

type logShip struct {
	Name    string
	Glob    string
	Service string // optional — "" derives from the file

	files map[string]*logshipFile // path -> tail state
}

type logshipFile struct {
	offset int64
	primed bool // first sight seeks to EOF
}

// parseLogship parses one `[logs]` entry: `name = <file-or-glob> | <service>`.
func parseLogship(name, spec string) (*logShip, error) {
	parts := strings.SplitN(spec, "|", 2)
	glob := strings.TrimSpace(parts[0])
	if glob == "" {
		return nil, fmt.Errorf("want `file-or-glob | service`, got %q", spec)
	}
	svc := ""
	if len(parts) == 2 {
		svc = logshipServiceName(strings.TrimSpace(parts[1]))
	}
	return &logShip{Name: name, Glob: glob, Service: svc, files: map[string]*logshipFile{}}, nil
}

// logshipServiceName mirrors the server's normalization: lowercase, a-z 0-9
// dot dash, spaces and underscores become dashes, 40 chars.
func logshipServiceName(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	var b strings.Builder
	for _, c := range s {
		switch {
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9', c == '-', c == '.':
			b.WriteRune(c)
		case c == ' ' || c == '_':
			b.WriteRune('-')
		}
		if b.Len() >= 40 {
			break
		}
	}
	return b.String()
}

// criName pulls the container name out of the Kubernetes log filename
// convention: /var/log/containers/<pod>_<namespace>_<container>-<64hex>.log
var criHash = regexp.MustCompile(`-[0-9a-f]{64}$`)

func logshipDeriveService(path string) string {
	base := strings.TrimSuffix(filepath.Base(path), ".log")
	if parts := strings.Split(base, "_"); len(parts) == 3 {
		return logshipServiceName(criHash.ReplaceAllString(parts[2], ""))
	}
	return logshipServiceName(base)
}

// logshipLevel is a light guess from the line itself — good enough to make
// severity filters useful, never authoritative.
func logshipLevel(line string) string {
	l := strings.ToLower(line)
	switch {
	case strings.Contains(l, "panic") || strings.Contains(l, "fatal"):
		return "fatal"
	case strings.Contains(l, "level=error") || strings.Contains(l, `"level":"error"`) ||
		strings.Contains(l, " error ") || strings.Contains(l, "error:") || strings.Contains(l, "[error]"):
		return "error"
	case strings.Contains(l, "level=warn") || strings.Contains(l, `"level":"warn"`) ||
		strings.Contains(l, " warn ") || strings.Contains(l, "warning") || strings.Contains(l, "[warn]"):
		return "warn"
	}
	return "info"
}

var criLine = regexp.MustCompile(`^(\S+) (stdout|stderr) [PF] (.*)$`)

// logshipUnwrap peels container encodings off a raw line, returning the
// message and the timestamp the runtime stamped it with (zero when absent).
func logshipUnwrap(line string) (string, time.Time) {
	if strings.HasPrefix(line, `{"log":`) {
		var d struct {
			Log  string `json:"log"`
			Time string `json:"time"`
		}
		if json.Unmarshal([]byte(line), &d) == nil && d.Log != "" {
			t, _ := time.Parse(time.RFC3339Nano, d.Time)
			return strings.TrimRight(d.Log, "\n"), t
		}
	}
	if m := criLine.FindStringSubmatch(line); m != nil {
		t, _ := time.Parse(time.RFC3339Nano, m[1])
		return m[3], t
	}
	return line, time.Time{}
}

type logshipRow struct {
	Msg     string `json:"msg"`
	Level   string `json:"level"`
	Service string `json:"service"`
	TS      int64  `json:"ts,omitempty"`
}

// collect reads what's new under this entry's glob and returns ready-to-ship
// rows. Offsets advance as we read: under a shedding server this cycle's
// lines are gone, by design (see the file header).
func (ls *logShip) collect() []logshipRow {
	paths, _ := filepath.Glob(ls.Glob)
	rows := []logshipRow{}
	seen := map[string]bool{}
	for _, path := range paths {
		seen[path] = true
		st := ls.files[path]
		if st == nil {
			st = &logshipFile{}
			ls.files[path] = st
		}
		fi, err := os.Stat(path)
		if err != nil {
			continue
		}
		if !st.primed {
			// first sight: start at the end — never replay history
			st.offset, st.primed = fi.Size(), true
			continue
		}
		if fi.Size() < st.offset {
			st.offset = 0 // rotated or truncated: new file, read from the top
		}
		if fi.Size() == st.offset {
			continue
		}
		f, err := os.Open(path)
		if err != nil {
			continue
		}
		if _, err := f.Seek(st.offset, io.SeekStart); err != nil {
			f.Close()
			continue
		}
		rd := bufio.NewReaderSize(io.LimitReader(f, logshipMaxScan), 64<<10)
		read := int64(0)
		for {
			line, err := rd.ReadString('\n')
			read += int64(len(line))
			line = strings.TrimRight(line, "\r\n")
			if line != "" && err == nil { // a partial last line waits for its newline
				msg, ts := logshipUnwrap(line)
				if len(msg) > logshipLineMax {
					msg = msg[:logshipLineMax] + "…[truncated]"
				}
				if msg != "" {
					svc := ls.Service
					if svc == "" {
						svc = logshipDeriveService(path)
					}
					row := logshipRow{Msg: msg, Level: logshipLevel(msg), Service: svc}
					if !ts.IsZero() {
						row.TS = ts.Unix()
					}
					rows = append(rows, row)
				}
			}
			if err != nil {
				read -= int64(len(line)) // leave the partial for next cycle
				break
			}
			if len(rows) >= logshipBatchRows {
				break
			}
		}
		st.offset += read
		f.Close()
		if len(rows) >= logshipBatchRows {
			break
		}
	}
	// forget files that left the glob (rotated away, pod gone)
	for p := range ls.files {
		if !seen[p] {
			delete(ls.files, p)
		}
	}
	return rows
}

// shipLogs runs every beacon tick: gather new lines across all [logs] entries
// and post them as one JSON-lines batch. Best-effort — a refusal costs the
// batch, never the check-in.
func shipLogs(cfg *config, client *http.Client) {
	if len(cfg.Logs) == 0 {
		return
	}
	var buf bytes.Buffer
	n := 0
	for _, ls := range cfg.Logs {
		for _, row := range ls.collect() {
			b, err := json.Marshal(row)
			if err != nil {
				continue
			}
			if buf.Len()+len(b)+1 > logshipBatchMax || n >= logshipBatchRows {
				break
			}
			buf.Write(b)
			buf.WriteByte('\n')
			n++
		}
	}
	if n == 0 {
		return
	}
	req, err := http.NewRequest("POST", cfg.URL+"/api/ingest/logs", &buf)
	if err != nil {
		return
	}
	req.Header.Set("Authorization", "Bearer "+cfg.Key)
	resp, err := client.Do(req)
	if err != nil {
		return // the next tick tries again with whatever is new by then
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
}
