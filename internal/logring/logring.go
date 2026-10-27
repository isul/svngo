// Package logring keeps the last N log lines in memory for the web UI.
package logring

import (
	"bytes"
	"sync"
)

type Ring struct {
	mu    sync.Mutex
	lines []string
	next  int
	full  bool
	part  []byte
}

func New(n int) *Ring { return &Ring{lines: make([]string, n)} }

func (r *Ring) Add(line string) {
	r.mu.Lock()
	r.lines[r.next] = line
	r.next = (r.next + 1) % len(r.lines)
	if r.next == 0 {
		r.full = true
	}
	r.mu.Unlock()
}

// Write implements io.Writer (used as slog output).
func (r *Ring) Write(p []byte) (int, error) {
	r.mu.Lock()
	r.part = append(r.part, p...)
	var done []string
	for {
		i := bytes.IndexByte(r.part, '\n')
		if i < 0 {
			break
		}
		done = append(done, string(r.part[:i]))
		r.part = r.part[i+1:]
	}
	r.mu.Unlock()
	for _, l := range done {
		r.Add(l)
	}
	return len(p), nil
}

// Lines returns lines newest first.
func (r *Ring) Lines(max int) []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := r.next
	if r.full {
		n = len(r.lines)
	}
	if max > n {
		max = n
	}
	out := make([]string, 0, max)
	for i := 1; i <= max; i++ {
		idx := (r.next - i + len(r.lines)) % len(r.lines)
		out = append(out, r.lines[idx])
	}
	return out
}
