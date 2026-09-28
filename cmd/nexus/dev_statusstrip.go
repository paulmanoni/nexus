package main

import (
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"
)

// statusStrip pins ONE line of abnormal state to the bottom of the `nexus
// dev` console — the buildkit/npm pattern: logs scroll above, the strip is
// redrawn beneath every write, so a down resource or an unresolved frontend
// warning can't scroll out of sight. It renders nothing while everything is
// healthy, and is disabled entirely off-tty (pipes keep clean output).
//
// Mechanics: every producer (app prettifier, [web] writer, banners) writes
// through Wrap. Each write erases the strip line, emits the log bytes, and —
// when the write ended on a newline — redraws the strip without one, leaving
// the cursor parked on it for the next erase.
type statusStrip struct {
	mu      sync.Mutex
	out     io.Writer // the terminal the strip is drawn on
	enabled bool
	items   map[string]string
	drawn   bool
}

func newStatusStrip(out io.Writer, enabled bool) *statusStrip {
	return &statusStrip{out: out, enabled: enabled, items: map[string]string{}}
}

// Wrap routes a log destination through the strip's erase/redraw cycle.
func (s *statusStrip) Wrap(w io.Writer) io.Writer {
	if s == nil || !s.enabled {
		return w
	}
	return &stripWriter{s: s, w: w}
}

type stripWriter struct {
	s *statusStrip
	w io.Writer
}

func (sw *stripWriter) Write(p []byte) (int, error) {
	sw.s.mu.Lock()
	defer sw.s.mu.Unlock()
	sw.s.eraseLocked()
	n, err := sw.w.Write(p)
	if err == nil && n > 0 && p[n-1] == '\n' {
		sw.s.redrawLocked()
	}
	return n, err
}

// Set puts (or replaces) one keyed entry on the strip and redraws.
func (s *statusStrip) Set(key, text string) {
	if s == nil || !s.enabled {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.items[key] == text {
		return
	}
	s.items[key] = text
	s.eraseLocked()
	s.redrawLocked()
}

// Clear removes a keyed entry and redraws (to nothing, when it was the last).
func (s *statusStrip) Clear(key string) {
	if s == nil || !s.enabled {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.items[key]; !ok {
		return
	}
	delete(s.items, key)
	s.eraseLocked()
	s.redrawLocked()
}

func (s *statusStrip) eraseLocked() {
	if !s.drawn {
		return
	}
	fmt.Fprint(s.out, "\r\x1b[2K")
	s.drawn = false
}

func (s *statusStrip) redrawLocked() {
	if len(s.items) == 0 {
		return
	}
	keys := make([]string, 0, len(s.items))
	for k := range s.items {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, s.items[k])
	}
	line := strings.Join(parts, " · ")
	if len(line) > 160 {
		line = line[:159] + "…"
	}
	fmt.Fprintf(s.out, "%s%s%s%s", ansiBold, ansiYellow, line, ansiReset)
	s.drawn = true
}
