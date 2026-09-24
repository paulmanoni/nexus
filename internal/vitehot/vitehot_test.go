package vitehot

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func write(t *testing.T, dist, body string) {
	t.Helper()
	p := Path(dist)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func on() bool { return true }

func TestAbsentFileMeansNoDevServer(t *testing.T) {
	h, err := NewReader(t.TempDir(), on).Current()
	if h != nil || err != nil {
		t.Fatalf("want (nil, nil), got (%v, %v)", h, err)
	}
}

func TestDisabledIgnoresAPresentFile(t *testing.T) {
	dist := t.TempDir()
	write(t, dist, `{"version":1,"origin":"http://127.0.0.1:5173","base":"/","entries":["src/main.ts"],"pid":0}`)
	h, err := NewReader(dist, func() bool { return false }).Current()
	if h != nil || err != nil {
		t.Fatalf("a disabled reader must ignore the file, got (%v, %v)", h, err)
	}
}

func TestReadsAndResolvesURLs(t *testing.T) {
	dist := t.TempDir()
	write(t, dist, `{"version":1,"origin":"http://127.0.0.1:5174/","base":"/app","entries":["src/main.ts"],"pid":0}`)
	h, err := NewReader(dist, on).Current()
	if err != nil || h == nil {
		t.Fatalf("got (%v, %v)", h, err)
	}
	if got := h.ClientURL(); got != "http://127.0.0.1:5174/app/@vite/client" {
		t.Fatalf("ClientURL = %q", got)
	}
	if got := h.URL(h.Entry()); got != "http://127.0.0.1:5174/app/src/main.ts" {
		t.Fatalf("entry URL = %q", got)
	}
}

func TestPicksUpARestartOnADifferentPort(t *testing.T) {
	dist := t.TempDir()
	r := NewReader(dist, on)
	write(t, dist, `{"version":1,"origin":"http://127.0.0.1:5173","entries":["src/main.ts"]}`)
	if h, _ := r.Current(); h == nil || h.Origin != "http://127.0.0.1:5173" {
		t.Fatalf("first read: %+v", h)
	}
	// Same size on purpose, so only the mtime tells the reader it changed.
	time.Sleep(20 * time.Millisecond)
	write(t, dist, `{"version":1,"origin":"http://127.0.0.1:5199","entries":["src/main.ts"]}`)
	now := time.Now().Add(time.Second)
	_ = os.Chtimes(Path(dist), now, now)
	if h, _ := r.Current(); h == nil || h.Origin != "http://127.0.0.1:5199" {
		t.Fatalf("restart not picked up: %+v", h)
	}
}

func TestRemovalIsNoticed(t *testing.T) {
	dist := t.TempDir()
	r := NewReader(dist, on)
	write(t, dist, `{"version":1,"origin":"http://127.0.0.1:5173"}`)
	if h, _ := r.Current(); h == nil {
		t.Fatal("expected a hot file")
	}
	_ = os.Remove(Path(dist))
	if h, err := r.Current(); h != nil || err != nil {
		t.Fatalf("after removal want (nil, nil), got (%v, %v)", h, err)
	}
}

func TestUnusableFilesAreErrorsNotSilence(t *testing.T) {
	cases := map[string]string{
		"malformed":      `{"version":1,"origin":`,
		"wrong version":  `{"version":99,"origin":"http://x"}`,
		"missing origin": `{"version":1}`,
	}
	for name, body := range cases {
		dist := t.TempDir()
		write(t, dist, body)
		h, err := NewReader(dist, on).Current()
		if h != nil || err == nil {
			t.Fatalf("%s: want an error, got (%v, %v)", name, h, err)
		}
	}
}

func TestStaleFileFromADeadProcess(t *testing.T) {
	if !alive(os.Getpid()) {
		t.Skip("no liveness check on this platform")
	}
	dist := t.TempDir()
	// A pid far above any real one on a test machine.
	write(t, dist, `{"version":1,"origin":"http://127.0.0.1:5173","pid":2147480000}`)
	h, err := NewReader(dist, on).Current()
	if h != nil || !errors.Is(err, ErrStale) {
		t.Fatalf("want ErrStale, got (%v, %v)", h, err)
	}
	if !strings.Contains(err.Error(), "delete the file") {
		t.Fatalf("stale error should say what to do: %v", err)
	}
}

func TestEnabledRule(t *testing.T) {
	cases := []struct {
		dev  bool
		env  string
		want bool
	}{
		{true, "", true},
		{false, "development", true},
		{false, "Development", true},
		{false, "production", false},
		{false, "", false},
		{false, "staging", false},
	}
	for _, c := range cases {
		if got := Enabled(c.dev, c.env); got != c.want {
			t.Errorf("Enabled(%v, %q) = %v, want %v", c.dev, c.env, got, c.want)
		}
	}
}

func TestSameLengthRewriteIsSeenImmediately(t *testing.T) {
	dist := t.TempDir()
	r := NewReader(dist, on)
	write(t, dist, `{"version":1,"origin":"http://127.0.0.1:5173"}`)
	if h, _ := r.Current(); h == nil || h.Origin != "http://127.0.0.1:5173" {
		t.Fatalf("first read: %+v", h)
	}
	// Same byte length, written back to back: a cache keyed on mtime and
	// size could return the old origin here on a coarse-timestamp filesystem.
	write(t, dist, `{"version":1,"origin":"http://127.0.0.1:5174"}`)
	if h, _ := r.Current(); h == nil || h.Origin != "http://127.0.0.1:5174" {
		t.Fatalf("same-length rewrite missed: %+v", h)
	}
}

func TestModuleEntrySkipsHTML(t *testing.T) {
	cases := []struct {
		entries []string
		want    string
	}{
		{[]string{"index.html"}, ""},
		{[]string{"index.html", "src/main.ts"}, "src/main.ts"},
		{[]string{"INDEX.HTML", "src/app.tsx"}, "src/app.tsx"},
		{[]string{"src/main.ts"}, "src/main.ts"},
		{nil, ""},
	}
	for _, c := range cases {
		h := &Hot{Entries: c.entries}
		if got := h.ModuleEntry(); got != c.want {
			t.Errorf("ModuleEntry(%v) = %q, want %q", c.entries, got, c.want)
		}
	}
}

func TestPathIsAbsolute(t *testing.T) {
	if p := NewReader("web/dist", on).Path(); !filepath.IsAbs(p) {
		t.Fatalf("Path() = %q, want absolute so messages name one file", p)
	}
}

func TestAbsoluteBaseUsesOnlyItsPathInDev(t *testing.T) {
	h := &Hot{Origin: "http://localhost:5174", Base: "https://cdn.example.com/app/"}
	if got := h.URL("src/main.ts"); got != "http://localhost:5174/app/src/main.ts" {
		t.Fatalf("URL = %q", got)
	}
}
