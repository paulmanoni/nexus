package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"time"
)

// buildState tells the running app where the dev loop's build stands —
// building, failed with the compiler's output, or done — through a file
// in the session's state directory, which the debug toolbar reads
// (dev.BuildState): the browser shows a rebuild while the previous build
// keeps serving.
type buildState struct{ path string }

func newBuildState(devStatePath string) buildState {
	if devStatePath == "" {
		return buildState{}
	}
	return buildState{path: filepath.Join(filepath.Dir(devStatePath), "build.json")}
}

func (s buildState) set(state string, since time.Time, output string) {
	if s.path == "" {
		return
	}
	b, err := json.Marshal(struct {
		State  string `json:"state"`
		Since  int64  `json:"since"`
		Output string `json:"output,omitempty"`
	}{state, since.UnixMilli(), output})
	if err != nil {
		return
	}
	tmp := s.path + ".tmp"
	if os.WriteFile(tmp, b, 0o600) == nil {
		_ = os.Rename(tmp, s.path)
	}
}

// tail keeps the last n bytes written to it: a build's output for the toolbar.
type tail struct {
	buf bytes.Buffer
	n   int
}

func (t *tail) Write(p []byte) (int, error) {
	t.buf.Write(p)
	if over := t.buf.Len() - t.n; over > 0 {
		t.buf.Next(over)
	}
	return len(p), nil
}

var ansiCode = regexp.MustCompile(`\x1b\[[0-9;]*m`)

func (t *tail) String() string { return ansiCode.ReplaceAllString(t.buf.String(), "") }
