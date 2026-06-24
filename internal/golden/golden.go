// Package golden is a tiny golden-file helper for the framework's code
// generators (client SDK, decorator codegen, schema walk). Golden tests snapshot
// a generator's FULL output to a file under testdata/ and byte-compare on each
// run, catching unintended formatting/ordering/whitespace drift that targeted
// substring assertions miss.
//
// Update goldens after an intentional generator change by re-running the tests
// with UPDATE_GOLDEN set:
//
//	UPDATE_GOLDEN=1 go test ./client/... ./internal/handlergen/... ./registry/...
//
// then review the testdata/ diff before committing.
package golden

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

// Assert compares got against the golden file testdata/<name>. With UPDATE_GOLDEN
// set it (re)writes the file instead and passes. A missing golden (without
// UPDATE_GOLDEN) fails with the exact command to create it.
func Assert(t *testing.T, got []byte, name string) {
	t.Helper()
	path := filepath.Join("testdata", name)

	if os.Getenv("UPDATE_GOLDEN") != "" {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("golden: mkdir: %v", err)
		}
		if err := os.WriteFile(path, got, 0o644); err != nil {
			t.Fatalf("golden: write %s: %v", path, err)
		}
		return
	}

	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("golden: read %s: %v\n(run `UPDATE_GOLDEN=1 go test ./...` to create it)", path, err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("output does not match %s (run `UPDATE_GOLDEN=1 go test` to update)\n%s",
			path, lineDiff(want, got))
	}
}

// AssertString is the string convenience wrapper around Assert.
func AssertString(t *testing.T, got, name string) {
	t.Helper()
	Assert(t, []byte(got), name)
}

// lineDiff renders the first differing line plus a small window of context so a
// failure points at WHAT drifted rather than dumping two whole files.
func lineDiff(want, got []byte) string {
	wl := bytes.Split(want, []byte("\n"))
	gl := bytes.Split(got, []byte("\n"))
	n := len(wl)
	if len(gl) > n {
		n = len(gl)
	}
	var b bytes.Buffer
	for i := 0; i < n; i++ {
		var w, g []byte
		if i < len(wl) {
			w = wl[i]
		}
		if i < len(gl) {
			g = gl[i]
		}
		if !bytes.Equal(w, g) {
			b.WriteString("first diff at line ")
			b.WriteString(itoa(i + 1))
			b.WriteString(":\n  want: ")
			b.Write(w)
			b.WriteString("\n  got:  ")
			b.Write(g)
			b.WriteString("\n")
			return b.String()
		}
	}
	return "(files differ only in trailing bytes / length)"
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}
