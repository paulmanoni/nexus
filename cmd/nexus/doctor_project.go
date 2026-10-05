package main

import (
	"fmt"
	"go/version"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"golang.org/x/mod/modfile"

	"github.com/paulmanoni/nexus/v2/config"
	"github.com/paulmanoni/nexus/v2/view/viewgen"
)

// doctorCheck is one line of `nexus doctor`'s project report.
type doctorCheck struct {
	Name   string
	Level  int // checkOK, checkWarn or checkFail
	Detail string
	Fix    string // what to do, for a warning or failure
}

const (
	checkOK = iota
	checkWarn
	checkFail
)

// projectChecks inspects the toolchain and the project in dir: what
// `nexus dev` and `nexus build` will need, checked before they fail.
func projectChecks(dir string) []doctorCheck {
	var out []doctorCheck
	add := func(c doctorCheck) { out = append(out, c) }

	// Go: installed, and new enough for go.mod.
	goVer, err := exec.Command("go", "env", "GOVERSION").Output()
	installed := strings.TrimSpace(string(goVer))
	var mf *modfile.File
	if raw, rerr := os.ReadFile(filepath.Join(dir, "go.mod")); rerr == nil {
		mf, _ = modfile.Parse("go.mod", raw, nil)
	}
	switch {
	case err != nil:
		add(doctorCheck{Name: "go", Level: checkFail, Detail: "the go command is not on PATH", Fix: "install Go from https://go.dev/dl"})
	case mf != nil && mf.Go != nil && version.Compare(installed, "go"+mf.Go.Version) < 0:
		add(doctorCheck{Name: "go", Level: checkFail, Detail: installed + ", go.mod asks for go " + mf.Go.Version, Fix: "install Go " + mf.Go.Version + " or newer"})
	default:
		add(doctorCheck{Name: "go", Detail: installed})
	}

	// The module: on nexus v2.
	if mf == nil {
		add(doctorCheck{Name: "module", Level: checkWarn, Detail: "no go.mod here", Fix: "run nexus doctor from the project root"})
	} else {
		var v1, v2 string
		for _, r := range mf.Require {
			switch r.Mod.Path {
			case "github.com/paulmanoni/nexus":
				v1 = r.Mod.Version
			case "github.com/paulmanoni/nexus/v2":
				v2 = r.Mod.Version
			}
		}
		switch {
		case v2 != "":
			add(doctorCheck{Name: "module", Detail: mf.Module.Mod.Path + " on nexus " + v2})
		case v1 != "":
			add(doctorCheck{Name: "module", Level: checkWarn, Detail: "on nexus " + v1, Fix: "move to v2 with nexus migrate v2"})
		default:
			add(doctorCheck{Name: "module", Level: checkWarn, Detail: mf.Module.Mod.Path + " doesn't require nexus"})
		}
	}

	// nexus.toml: present and valid under the strict rules.
	cfgPath := filepath.Join(dir, config.DefaultPath)
	if _, err := os.Stat(cfgPath); err != nil {
		add(doctorCheck{Name: "nexus.toml", Level: checkWarn, Detail: "none — the app runs on framework defaults", Fix: "nexus new writes one; see nexus docs nexustoml"})
	} else {
		declareProjectConfig(dir)
		issues, err := config.LintFile(cfgPath)
		errs, warns := count(issues)
		switch {
		case err != nil:
			add(doctorCheck{Name: "nexus.toml", Level: checkFail, Detail: err.Error(), Fix: "nexus config check shows each problem"})
		case errs > 0:
			add(doctorCheck{Name: "nexus.toml", Level: checkFail, Detail: countOf(errs, "error") + ", " + countOf(warns, "warning"), Fix: "nexus config check shows each problem"})
		case warns > 0:
			add(doctorCheck{Name: "nexus.toml", Level: checkWarn, Detail: countOf(warns, "warning"), Fix: "nexus config check shows each"})
		default:
			add(doctorCheck{Name: "nexus.toml", Detail: "valid"})
		}
	}

	// The frontend: Node 20+, its package manager, Vite installed.
	feDir, source := resolveFrontendDir(dir, "")
	if inspectFrontend(feDir).PackageJSON {
		rel, _ := filepath.Rel(dir, feDir)
		nodeOut, err := exec.Command("node", "--version").Output()
		nodeVer := strings.TrimSpace(string(nodeOut))
		major, _ := strconv.Atoi(strings.SplitN(strings.TrimPrefix(nodeVer, "v"), ".", 2)[0])
		switch {
		case err != nil:
			add(doctorCheck{Name: "node", Level: checkFail, Detail: "not on PATH — " + rel + " is a Vite project", Fix: "install Node.js 20 or newer"})
		case major < 20:
			add(doctorCheck{Name: "node", Level: checkFail, Detail: nodeVer + " — Vite needs 20 or newer", Fix: "install Node.js 20 or newer"})
		default:
			add(doctorCheck{Name: "node", Detail: nodeVer})
		}
		pm := detectPackageManager(feDir)
		if _, err := exec.LookPath(pm.Name); err != nil {
			add(doctorCheck{Name: "package manager", Level: checkFail, Detail: pm.Name + " (chosen by " + orDefault(pm.Lockfile, "package.json") + ") is not on PATH", Fix: "install " + pm.Name})
		} else {
			add(doctorCheck{Name: "package manager", Detail: pm.Name})
		}
		if _, err := os.Stat(filepath.Join(feDir, "node_modules", ".bin", "vite")); err != nil {
			add(doctorCheck{Name: "frontend", Level: checkWarn, Detail: rel + " (" + source + "): dependencies not installed", Fix: "nexus dev installs them on first run"})
		} else {
			add(doctorCheck{Name: "frontend", Detail: rel + " (" + source + ")"})
		}
	}

	// Tailwind: the CLI, when a stylesheet imports it.
	if entries := findTailwindEntries(dir, feDir); len(entries) > 0 {
		if _, err := tailwindBinary(); err != nil {
			add(doctorCheck{Name: "tailwind", Level: checkFail, Detail: countOf(len(entries), "stylesheet") + " import tailwindcss, but the CLI is not on PATH", Fix: "install the standalone tailwindcss binary"})
		} else {
			add(doctorCheck{Name: "tailwind", Detail: countOf(len(entries), "stylesheet")})
		}
	}

	// Views: the generated Go matches the .templ sources.
	if viewgen.HasTemplates(dir) {
		if err := checkViews(dir, io.Discard); err != nil {
			add(doctorCheck{Name: "views", Level: checkWarn, Detail: "generated files are out of date", Fix: "nexus generate views (nexus dev does it on start)"})
		} else {
			add(doctorCheck{Name: "views", Detail: "generated files up to date"})
		}
		if plan, err := viewgen.Generate(dir); err == nil && len(plan.Warnings) > 0 {
			add(doctorCheck{Name: "live pages", Level: checkWarn,
				Detail: fmt.Sprintf("%d place(s) keep a live page from rendering only what changed — first: %s", len(plan.Warnings), viewsError(dir, plan.Warnings[0])),
				Fix:    "keep a live page's state in view.Assign fields, read in Render (nexus lsp marks each place)"})
		}
	}

	// auth: where issued tokens and session records live.
	if uses, store := authTokenStore(dir); uses {
		if store == "" {
			add(doctorCheck{Name: "auth tokens", Level: checkWarn,
				Detail: "kept in memory — lost on restart and unknown to other replicas",
				Fix:    "authdb.Bind[DB]() (extension/auth/authdb), or auth.Config{Tokens: auth.CacheTokens(cache)}"})
		} else {
			add(doctorCheck{Name: "auth tokens", Detail: store})
		}
	}
	return out
}

// authTokenStore reports whether the app's Go source uses extension/auth,
// and which durable token store it wires ("" for none: the memory store).
func authTokenStore(dir string) (uses bool, store string) {
	_ = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			name := d.Name()
			if p != dir && (strings.HasPrefix(name, ".") || name == "node_modules" || name == "vendor" || name == "testdata") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
			return nil
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return nil
		}
		src := string(b)
		if strings.Contains(src, `nexus/v2/extension/auth"`) || strings.Contains(src, `nexus/v2/extension/auth/authdb"`) {
			uses = true
		}
		switch {
		case strings.Contains(src, "authdb.Bind"):
			store = "authdb (SQL database)"
		case strings.Contains(src, "CacheTokens("):
			store = "auth.CacheTokens (cache)"
		case store == "" && strings.Contains(src, "Tokens:"):
			store = "a custom auth.TokenStore"
		}
		return nil
	})
	return uses, store
}

func orDefault(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

// emitProjectChecks prints the report and reports whether anything failed.
func emitProjectChecks(w io.Writer, checks []doctorCheck) (failed bool) {
	marks := []string{ansiGreen + "✓" + ansiReset, ansiYellow + "!" + ansiReset, ansiRed + "✗" + ansiReset}
	for _, c := range checks {
		fmt.Fprintf(w, "  %s %-16s %s\n", marks[c.Level], c.Name, c.Detail)
		if c.Level != checkOK && c.Fix != "" {
			fmt.Fprintf(w, "    %s→ %s%s\n", ansiDim, c.Fix, ansiReset)
		}
		if c.Level == checkFail {
			failed = true
		}
	}
	return failed
}

// countOf is "1 error", "3 errors".
func countOf(n int, word string) string {
	if n == 1 {
		return "1 " + word
	}
	return strconv.Itoa(n) + " " + word + "s"
}
