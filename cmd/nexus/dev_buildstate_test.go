package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestBuildState(t *testing.T) {
	dir := t.TempDir()
	s := newBuildState(filepath.Join(dir, "state.json"))
	out := &tail{n: 32}
	out.Write([]byte("\x1b[31mmain.go:3:1: undefined: x\x1b[0m\n"))
	out.Write([]byte("main.go:9:2: undefined: y\n"))
	s.set("failed", time.UnixMilli(1000), out.String())

	b, err := os.ReadFile(filepath.Join(dir, "build.json"))
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		State, Output string
		Since         int64
	}
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	if got.State != "failed" || got.Since != 1000 || strings.Contains(got.Output, "\x1b") || !strings.HasSuffix(got.Output, "undefined: y\n") || len(got.Output) > 32 {
		t.Fatalf("build state = %+v", got)
	}
	newBuildState("").set("ok", time.Now(), "") // no session: nothing written, no panic
}
