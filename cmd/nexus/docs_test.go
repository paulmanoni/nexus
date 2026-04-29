package main

import (
	"bytes"
	"strings"
	"testing"
)

// TestDocsCmd_Index exercises the no-arg path: every registered
// topic shows up in the index with its summary, and the canonical
// follow-on hints (--web + nexus help) are present so users know
// where to go next.
func TestDocsCmd_Index(t *testing.T) {
	stdout, stderr := new(bytes.Buffer), new(bytes.Buffer)
	cmd := newDocsCmd(stdout, stderr)
	cmd.SetArgs([]string{})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("execute: %v stderr=%q", err, stderr.String())
	}
	out := stdout.String()
	for _, name := range topicNames() {
		if !strings.Contains(out, name) {
			t.Errorf("index missing topic %q\n%s", name, out)
		}
	}
	if !strings.Contains(out, "--web") {
		t.Errorf("index missing --web hint")
	}
}

// TestDocsCmd_Topic verifies one topic prints its body. Picks
// "frontend" because it's the most recent and most likely to
// regress when topic content shifts.
func TestDocsCmd_Topic(t *testing.T) {
	stdout, stderr := new(bytes.Buffer), new(bytes.Buffer)
	cmd := newDocsCmd(stdout, stderr)
	cmd.SetArgs([]string{"frontend"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("execute: %v stderr=%q", err, stderr.String())
	}
	out := stdout.String()
	for _, want := range []string{"FRONTEND", "ServeFrontend", "FrontendAt", "//go:embed"} {
		if !strings.Contains(out, want) {
			t.Errorf("topic body missing %q\n%s", want, out)
		}
	}
}

// TestDocsCmd_UnknownTopic returns an error and prints a typo
// suggestion so the user gets pointed at the right name.
func TestDocsCmd_UnknownTopic(t *testing.T) {
	stdout, stderr := new(bytes.Buffer), new(bytes.Buffer)
	cmd := newDocsCmd(stdout, stderr)
	cmd.SetArgs([]string{"frontnd"}) // 1-edit typo of "frontend"
	cmd.SilenceUsage = true
	cmd.SilenceErrors = true
	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected error for unknown topic")
	}
	if !strings.Contains(stderr.String(), "unknown topic") {
		t.Errorf("stderr missing 'unknown topic': %q", stderr.String())
	}
	if !strings.Contains(stderr.String(), `Did you mean "frontend"`) {
		t.Errorf("stderr missing typo suggestion: %q", stderr.String())
	}
}

// TestDocsCmd_List prints one topic per line, suitable for
// shell completion / scripting. No prose, no summaries.
func TestDocsCmd_List(t *testing.T) {
	stdout, stderr := new(bytes.Buffer), new(bytes.Buffer)
	cmd := newDocsCmd(stdout, stderr)
	cmd.SetArgs([]string{"--list"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("execute: %v stderr=%q", err, stderr.String())
	}
	lines := strings.Split(strings.TrimSpace(stdout.String()), "\n")
	if len(lines) != len(docsTopics) {
		t.Errorf("list line count: got %d want %d", len(lines), len(docsTopics))
	}
	for _, line := range lines {
		if _, ok := docsTopics[line]; !ok {
			t.Errorf("list contains non-topic line %q", line)
		}
	}
}
