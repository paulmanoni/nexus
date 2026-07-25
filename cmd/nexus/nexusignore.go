package main

import (
	"bufio"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// nexusIgnoreFile is the per-project ignore list `nexus dev` reads at
// startup. It sits next to nexus.toml.
const nexusIgnoreFile = ".nexusignore"

// ignoreMatcher applies a .nexusignore file: gitignore-style patterns that
// keep paths out of the dev loop's watchers, so saves under them neither
// rebuild the app nor rebuild web/dist.
//
// The built-in skip list (.git, node_modules, vendor, testdata, …) already
// covers the usual noise; this is for what only the project knows —
// generated trees, fixtures, a scratch directory, a sibling service's
// source that happens to live in the repo.
//
// Syntax (a documented subset of .gitignore):
//
//	# comment                  comments and blank lines are ignored
//	tmp/                       a trailing slash matches directories only
//	generated                  no slash: matches at any depth, like gitignore
//	internal/mock/*.go         a slash anchors the pattern to the project root
//	assets/**/snapshots        ** spans any number of directories
//	!internal/mock/keep.go     ! re-includes; later rules win
//
// A directory that matches is pruned, so nothing inside it is watched at
// all — a re-include under a pruned directory can't resurrect it (same
// rule git applies, for the same reason: we never descend to find out).
//
// The file is read once at startup; editing it takes effect on the next
// `nexus dev`.
type ignoreMatcher struct {
	root  string
	rules []ignoreRule
}

type ignoreRule struct {
	segs     []string // pattern split on "/"
	anchored bool     // contains a slash: matched from the project root
	dirOnly  bool     // trailing slash: only directories match
	negate   bool     // leading "!": re-includes a path an earlier rule ignored
}

// loadNexusIgnore reads root/.nexusignore. Returns nil when the file is
// absent or holds no usable patterns, so callers can skip matching
// entirely in the common case.
func loadNexusIgnore(root string) *ignoreMatcher {
	f, err := os.Open(filepath.Join(root, nexusIgnoreFile))
	if err != nil {
		return nil
	}
	defer f.Close()
	return parseNexusIgnore(root, f)
}

func parseNexusIgnore(root string, r io.Reader) *ignoreMatcher {
	abs, err := filepath.Abs(root)
	if err != nil {
		abs = root
	}
	m := &ignoreMatcher{root: abs}
	scanner := bufio.NewScanner(r)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		var rule ignoreRule
		if strings.HasPrefix(line, "!") {
			rule.negate = true
			line = strings.TrimSpace(line[1:])
		}
		if strings.HasSuffix(line, "/") {
			rule.dirOnly = true
			line = strings.TrimSuffix(line, "/")
		}
		line = strings.Trim(line, "/")
		if line == "" {
			continue
		}
		rule.segs = strings.Split(line, "/")
		rule.anchored = len(rule.segs) > 1
		m.rules = append(m.rules, rule)
	}
	if len(m.rules) == 0 {
		return nil
	}
	return m
}

// patterns reports how many rules were loaded, for the startup line.
func (m *ignoreMatcher) patterns() int {
	if m == nil {
		return 0
	}
	return len(m.rules)
}

// match reports whether path (a file or directory, absolute or relative to
// the process's cwd) is ignored. Paths outside the project root never
// match. Later rules win, so a `!` re-include must follow the rule it
// overrides.
func (m *ignoreMatcher) match(path string, isDir bool) bool {
	if m == nil {
		return false
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return false
	}
	rel, err := filepath.Rel(m.root, abs)
	if err != nil || rel == "." || strings.HasPrefix(rel, "..") {
		return false
	}
	segs := strings.Split(filepath.ToSlash(rel), "/")

	ignored := false
	for _, rule := range m.rules {
		if rule.matches(segs, isDir) {
			ignored = !rule.negate
		}
	}
	return ignored
}

func (r ignoreRule) matches(segs []string, isDir bool) bool {
	if r.anchored {
		// Anchored patterns match the path itself or any ancestor of it —
		// matching an ancestor means the path lives inside an ignored tree.
		for i := 1; i <= len(segs); i++ {
			if r.dirOnly && i == len(segs) && !isDir {
				continue
			}
			if matchSegments(r.segs, segs[:i]) {
				return true
			}
		}
		return false
	}
	// Unanchored: a single component matched at any depth, like gitignore.
	for i, seg := range segs {
		last := i == len(segs)-1
		if r.dirOnly && last && !isDir {
			continue
		}
		if ok, err := filepath.Match(r.segs[0], seg); err == nil && ok {
			return true
		}
	}
	return false
}

// matchSegments matches a slash-split pattern against slash-split path
// segments, where "**" spans zero or more segments and every other segment
// follows filepath.Match.
func matchSegments(pat, segs []string) bool {
	if len(pat) == 0 {
		return len(segs) == 0
	}
	if pat[0] == "**" {
		for i := 0; i <= len(segs); i++ {
			if matchSegments(pat[1:], segs[i:]) {
				return true
			}
		}
		return false
	}
	if len(segs) == 0 {
		return false
	}
	if ok, err := filepath.Match(pat[0], segs[0]); err != nil || !ok {
		return false
	}
	return matchSegments(pat[1:], segs[1:])
}
