package dev

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"
)

// Build is where nexus dev's rebuild stands while this process serves:
// State "building" (Since it started), "failed" (Output is the compiler's)
// or "ok". The zero Build outside nexus dev, or before its first rebuild.
type Build struct {
	State  string    `json:"state"`
	Since  time.Time `json:"since"`
	Output string    `json:"output,omitempty"`
}

// BuildState reads the dev loop's build state.
func BuildState() Build {
	dir := StateDir()
	if dir == "" {
		return Build{}
	}
	b, err := os.ReadFile(filepath.Join(dir, "build.json"))
	if err != nil {
		return Build{}
	}
	var raw struct {
		State  string `json:"state"`
		Since  int64  `json:"since"`
		Output string `json:"output"`
	}
	if json.Unmarshal(b, &raw) != nil {
		return Build{}
	}
	return Build{State: raw.State, Since: time.UnixMilli(raw.Since), Output: raw.Output}
}
