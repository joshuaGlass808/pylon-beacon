// [logwatch] — count log-pattern matches in a rolling window, and carry the
// most recent matched block along with the push.
//
// The itch: "a few tracebacks are normal, a SPIKE is not — and when it spikes,
// the page should contain the traceback." Each entry tails a file, counts
// regex matches inside its window, and reports that count as a normal metric
// (so PylonMon vital rules like `tracebacks_5m > 20 sustained 60s` just work).
// The latest matched block rides in the push as a text sample, so a breach
// page can show the actual error instead of a number.
//
// Reading is incremental: we remember the byte offset between pushes and only
// scan what's new. On first start we seek to the END of the file — installing
// the beacon must never page you about last week's errors. Truncation or
// rotation (file smaller than our offset) restarts from the top of the new
// file.

package main

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const (
	logwatchDefaultWindow = 300     // seconds, when the third field is omitted
	logwatchMaxScan       = 4 << 20 // bytes read per cycle — survives log floods
	logwatchSampleLines   = 40      // block capture caps: lines…
	logwatchSampleBytes   = 4000    // …and bytes
)

type logWatch struct {
	Name   string
	File   string
	Re     *regexp.Regexp
	Window int // seconds

	// state between cycles
	offset int64
	primed bool    // first cycle done (we start at EOF, not at byte 0)
	hits   []int64 // unix timestamps of matches, pruned to Window
	sample string  // most recent captured block
}

// parseLogwatch parses one `[logwatch]` entry:
//
//	name = <file> | <regex> | <window seconds>
//
// The window is optional (default 300). The regex is Go syntax, matched per
// line; it may itself contain `|` — only a trailing purely-numeric field is
// taken as the window.
func parseLogwatch(name, spec string) (*logWatch, error) {
	parts := strings.Split(spec, "|")
	if len(parts) < 2 {
		return nil, fmt.Errorf("want `file | regex | window-seconds`, got %q", spec)
	}
	file := strings.TrimSpace(parts[0])
	window := logwatchDefaultWindow
	reParts := parts[1:]
	if len(parts) > 2 {
		if n, err := strconv.Atoi(strings.TrimSpace(parts[len(parts)-1])); err == nil {
			window = n
			reParts = parts[1 : len(parts)-1]
		}
	}
	reSrc := strings.TrimSpace(strings.Join(reParts, "|"))
	if file == "" || reSrc == "" {
		return nil, fmt.Errorf("file and regex are both required, got %q", spec)
	}
	re, err := regexp.Compile(reSrc)
	if err != nil {
		return nil, fmt.Errorf("bad regex %q: %v", reSrc, err)
	}
	if window < 15 {
		window = 15
	}
	return &logWatch{Name: name, File: file, Re: re, Window: window}, nil
}

// scan reads whatever the file grew since the last cycle, updates the rolling
// match count, and refreshes the captured sample. Any error (file missing,
// unreadable) reports the current window count and tries again next cycle —
// a broken watch must never break the push.
func (w *logWatch) scan(now int64) {
	defer w.prune(now)
	f, err := os.Open(w.File)
	if err != nil {
		return
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return
	}
	size := st.Size()
	if !w.primed {
		// first sight of the file: start at the end — history is not news
		w.offset, w.primed = size, true
		return
	}
	if size < w.offset {
		w.offset = 0 // truncated or rotated: the new file starts fresh
	}
	if size == w.offset {
		return
	}
	if _, err := f.Seek(w.offset, io.SeekStart); err != nil {
		return
	}
	// cap the read so a runaway log can't stall the push loop; we catch up
	// next cycle
	remaining := size - w.offset
	if remaining > logwatchMaxScan {
		remaining = logwatchMaxScan
	}
	r := bufio.NewReaderSize(io.LimitReader(f, remaining), 64*1024)

	var block []string // in-progress sample capture; nil = not capturing
	blockBytes := 0
	flush := func() {
		if len(block) > 0 {
			w.sample = strings.Join(block, "\n")
		}
		block, blockBytes = nil, 0
	}
	var consumed int64
	for {
		line, rerr := r.ReadString('\n')
		if !strings.HasSuffix(line, "\n") {
			// incomplete tail (mid-write, or we hit the read cap): leave it
			// un-consumed so next cycle matches the line whole
			break
		}
		consumed += int64(len(line))
		text := strings.TrimRight(line, "\r\n")

		if w.Re.MatchString(text) {
			// a new match always starts a new block (back-to-back tracebacks
			// each count — the previous capture closes as-is)
			flush()
			w.hits = append(w.hits, now)
			block = []string{text}
			blockBytes = len(text) + 1
		} else if block != nil {
			// Continuation lines are indented; the first NON-indented line
			// after them closes the block and is included — for a Python
			// traceback that line is the `SomeError: message` at the bottom.
			indented := strings.HasPrefix(text, " ") || strings.HasPrefix(text, "\t")
			block = append(block, text)
			blockBytes += len(text) + 1
			if (!indented && len(block) > 1) || len(block) >= logwatchSampleLines || blockBytes >= logwatchSampleBytes {
				flush()
			}
		}
		if rerr != nil {
			break
		}
	}
	flush()
	w.offset += consumed
}

// prune drops match timestamps that fell out of the window.
func (w *logWatch) prune(now int64) {
	cut := now - int64(w.Window)
	i := 0
	for i < len(w.hits) && w.hits[i] < cut {
		i++
	}
	w.hits = w.hits[i:]
}

// scanLogwatches runs every watch and returns the counts (merged into the
// metric map by the caller) plus the latest text sample per watch.
func scanLogwatches(watches []*logWatch) (map[string]float64, map[string]string) {
	if len(watches) == 0 {
		return nil, nil
	}
	now := time.Now().Unix()
	counts := make(map[string]float64, len(watches))
	samples := map[string]string{}
	for _, w := range watches {
		w.scan(now)
		counts[w.Name] = float64(len(w.hits))
		if w.sample != "" {
			s := w.sample
			if len(s) > logwatchSampleBytes {
				s = s[:logwatchSampleBytes]
			}
			samples[w.Name] = s
		}
	}
	return counts, samples
}
