package main

import (
	"bytes"
	"context"
	"path/filepath"
	"strings"
	"testing"
)

// viteBanner is what Vite 6 prints on start and after a config-triggered
// restart, colors included (FORCE_COLOR).
const viteBanner = "\n" +
	"  \x1b[32m\x1b[1mVITE\x1b[22m v6.4.3\x1b[39m  \x1b[2mready in \x1b[0m\x1b[1m312\x1b[22m\x1b[2m\x1b[0m ms\x1b[22m\n" +
	"\n" +
	"  \x1b[32m➜\x1b[39m  \x1b[1mLocal\x1b[22m:   \x1b[36mhttp://localhost:\x1b[1m5190\x1b[22m/\x1b[39m\n" +
	"  \x1b[32m➜\x1b[39m  \x1b[1mNetwork\x1b[22m\x1b[2m: use \x1b[22m\x1b[1m--host\x1b[22m\x1b[2m to expose\x1b[22m\n" +
	"  \x1b[32m➜\x1b[39m  \x1b[2mpress \x1b[22m\x1b[1mh + enter\x1b[22m\x1b[2m to show help\x1b[22m\n" +
	"[nexus] dev server http://[::1]:5190 → dist/.vite/nexus-hot.json\n" +
	"10:02:11 AM [vite] (client) hmr update /src/Pages/Home.vue\n" +
	"  ➜  Local:   http://localhost:5190/\n" +
	"  ➜  Network: http://192.168.1.4:5190/\n"

func TestViteLogWriter(t *testing.T) {
	var out bytes.Buffer
	w := newViteLogWriter(&out, false)
	// Split mid-line: lines are reassembled before they are judged.
	half := len(viteBanner) / 2
	w.Write([]byte(viteBanner[:half]))
	w.Write([]byte(viteBanner[half:]))
	w.Write([]byte("partial without newline"))
	w.flush()

	got := out.String()
	for _, hidden := range []string{"Local", "Network", "press", "h + enter", "5190/"} {
		if strings.Contains(got, hidden) {
			t.Errorf("filtered output still has %q:\n%s", hidden, got)
		}
	}
	lines := strings.Split(strings.TrimRight(got, "\n"), "\n")
	if len(lines) != 4 {
		t.Fatalf("got %d lines, want 4 (ready, plugin, hmr, partial):\n%s", len(lines), got)
	}
	for _, want := range []string{"VITE", "[nexus] dev server http://[::1]:5190", "hmr update /src/Pages/Home.vue", "partial without newline"} {
		if !strings.Contains(got, want) {
			t.Errorf("output lost %q:\n%s", want, got)
		}
	}
	for _, l := range lines {
		if !strings.Contains(l, "[web]") {
			t.Errorf("line not prefixed with [web]: %q", l)
		}
	}
}

func TestViteLogWriter_VerboseKeepsEverything(t *testing.T) {
	var out bytes.Buffer
	w := newViteLogWriter(&out, true)
	w.Write([]byte(viteBanner))
	if n := strings.Count(out.String(), "\n"); n != strings.Count(viteBanner, "\n") {
		t.Errorf("--verbose dropped lines: %d of %d\n%s", n, strings.Count(viteBanner, "\n"), out.String())
	}
	if !strings.Contains(out.String(), "Local") {
		t.Error("--verbose hid the Local line")
	}
}

func TestViteNoiseLine(t *testing.T) {
	noise := []string{"", "   ", "  ➜  Local:   http://localhost:5173/", "  ➜  Network: use --host to expose",
		"  ➜  press h + enter to show help", "\x1b[32m➜\x1b[39m  \x1b[1mLocal\x1b[22m: x"}
	for _, l := range noise {
		if !viteNoiseLine(l) {
			t.Errorf("viteNoiseLine(%q) = false, want true", l)
		}
	}
	kept := []string{"  VITE v6.4.3  ready in 300 ms", "error: Local variable x is unused",
		"[vite] Internal server error: Failed to resolve import", "Port 5190 is in use, trying another one..."}
	for _, l := range kept {
		if viteNoiseLine(l) {
			t.Errorf("viteNoiseLine(%q) = true, want false", l)
		}
	}
}

const serveFrontendMain = `package main

import (
	"embed"

	"github.com/paulmanoni/nexus"
)

//go:embed all:client/dist
var webFS embed.FS

func main() { nexus.Boot(nexus.ServeFrontend(webFS, "client/dist")) }
`

func TestResolveFrontendDir(t *testing.T) {
	t.Setenv("NEXUS_FRONTEND_DIR", "")

	t.Run("detected root is relative to the package, not the cwd", func(t *testing.T) {
		proj := t.TempDir()
		pkg := filepath.Join(proj, "cmd", "app")
		writeFile(t, filepath.Join(pkg, "main.go"), serveFrontendMain)
		t.Chdir(proj) // `nexus dev ./cmd/app` from the project root
		dir, source := resolveFrontendDir(pkg, "")
		if want := filepath.Join(pkg, "client"); dir != want {
			t.Fatalf("dir = %q, want %q", dir, want)
		}
		if source == "" {
			t.Error("no source reported")
		}
	})
	t.Run("--frontend is relative to the cwd", func(t *testing.T) {
		proj := t.TempDir()
		pkg := filepath.Join(proj, "cmd", "app")
		writeFile(t, filepath.Join(pkg, "main.go"), serveFrontendMain)
		t.Chdir(proj)
		dir, source := resolveFrontendDir(pkg, "./ui")
		if want := filepath.Join(proj, "ui"); dir != want || source != "--frontend" {
			t.Fatalf("got (%q, %q), want (%q, --frontend)", dir, source, want)
		}
	})
	t.Run("NEXUS_FRONTEND_DIR overrides detection, relative to the project", func(t *testing.T) {
		pkg := t.TempDir()
		writeFile(t, filepath.Join(pkg, "main.go"), serveFrontendMain)
		t.Setenv("NEXUS_FRONTEND_DIR", "frontend")
		dir, source := resolveFrontendDir(pkg, "")
		if want := filepath.Join(pkg, "frontend"); dir != want || source != "NEXUS_FRONTEND_DIR" {
			t.Fatalf("got (%q, %q), want (%q, NEXUS_FRONTEND_DIR)", dir, source, want)
		}
		abs := t.TempDir()
		t.Setenv("NEXUS_FRONTEND_DIR", abs)
		if dir, _ := resolveFrontendDir(pkg, ""); dir != abs {
			t.Fatalf("absolute NEXUS_FRONTEND_DIR: dir = %q, want %q", dir, abs)
		}
		if dir, _ := resolveFrontendDir(pkg, "/elsewhere"); dir != "/elsewhere" {
			t.Fatalf("--frontend must beat NEXUS_FRONTEND_DIR, got %q", dir)
		}
	})
	t.Run("web/package.json without a literal ServeFrontend root", func(t *testing.T) {
		pkg := t.TempDir()
		writeFile(t, filepath.Join(pkg, "main.go"), "package main\nfunc main() {}\n")
		if dir, _ := resolveFrontendDir(pkg, ""); dir != "" {
			t.Fatalf("no frontend, got %q", dir)
		}
		writeFile(t, filepath.Join(pkg, "web", "package.json"), "{}")
		if dir, _ := resolveFrontendDir(pkg, ""); dir != filepath.Join(pkg, "web") {
			t.Fatalf("dir = %q, want web", dir)
		}
	})
	t.Run("a viteless-era web/ is found, so it gets the migration hint", func(t *testing.T) {
		pkg := t.TempDir()
		writeFile(t, filepath.Join(pkg, "main.go"), "package main\nfunc main() {}\n")
		writeFile(t, filepath.Join(pkg, "web", "viteless.config.ts"), "")
		if dir, _ := resolveFrontendDir(pkg, ""); dir != filepath.Join(pkg, "web") {
			t.Fatalf("dir = %q, want web", dir)
		}
	})
}

func TestStartDevFrontend_NoViteWithoutPackageJSON(t *testing.T) {
	t.Setenv("NEXUS_FRONTEND_DIR", "")
	ctx := context.Background()

	t.Run("legacy viteless dir: hint, no dev server", func(t *testing.T) {
		pkg := t.TempDir()
		writeFile(t, filepath.Join(pkg, "main.go"), serveFrontendMain)
		writeFile(t, filepath.Join(pkg, "client", "viteless.config.ts"), "export default {}")
		var out, notes bytes.Buffer
		f := startDevFrontend(ctx, pkg, "", "", devViteConfig{Out: &out, Notes: &notes})
		if f.Vite != nil {
			f.Vite.stop()
			t.Fatal("started a dev server for a viteless-era dir")
		}
		if f.Dir != filepath.Join(pkg, "client") || f.Project.Legacy == "" {
			t.Fatalf("frontend = %+v", f)
		}
		if !strings.Contains(notes.String(), "viteless.config.ts") || !strings.Contains(notes.String(), "package.json") {
			t.Errorf("no migration hint: %q", notes.String())
		}
		if strings.Count(notes.String(), "\n") != 1 {
			t.Errorf("hint should be one line: %q", notes.String())
		}
	})
	t.Run("static bundle: silent, no dev server", func(t *testing.T) {
		pkg := t.TempDir()
		writeFile(t, filepath.Join(pkg, "main.go"), serveFrontendMain)
		writeFile(t, filepath.Join(pkg, "client", "dist", "index.html"), "<html>")
		var out, notes bytes.Buffer
		f := startDevFrontend(ctx, pkg, "", "", devViteConfig{Out: &out, Notes: &notes})
		if f.Vite != nil {
			f.Vite.stop()
			t.Fatal("started a dev server without a package.json")
		}
		if f.Dir == "" || notes.Len() != 0 {
			t.Fatalf("frontend = %+v, notes %q", f, notes.String())
		}
	})
}
