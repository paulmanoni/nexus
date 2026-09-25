//go:build !windows

package main

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// fakeInstallerBody records "<tool> <args>" in $INSTALL_LOG and installs a
// Vite, the way a real install would.
const fakeInstallerBody = `echo "$(basename "$0") $*" >> "$INSTALL_LOG"
mkdir -p node_modules/.bin
printf '#!/bin/sh\n' > node_modules/.bin/vite
chmod +x node_modules/.bin/vite
`

// fakeTools puts executables named tools on PATH (with the system
// directories the scripts need), each running body, and points
// $INSTALL_LOG at a log the test reads with installCalls.
func fakeTools(t *testing.T, body string, tools ...string) (log string) {
	t.Helper()
	bin := t.TempDir()
	for _, name := range tools {
		p := filepath.Join(bin, name)
		writeTestFile(t, p, "#!/bin/sh\n"+body)
		if err := os.Chmod(p, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", bin+":/usr/bin:/bin")
	log = filepath.Join(t.TempDir(), "install.log")
	t.Setenv("INSTALL_LOG", log)
	return log
}

func installCalls(t *testing.T, log string) []string {
	t.Helper()
	b, err := os.ReadFile(log)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimSpace(string(b)), "\n")
}

// A pnpm, Yarn or Bun project is installed with its own tool and lockfile
// — npm would ignore the lockfile and write a package-lock.json.
func TestEnsureNodeModules_UsesTheProjectsPackageManager(t *testing.T) {
	cases := []struct {
		files map[string]string
		want  string
	}{
		{map[string]string{"pnpm-lock.yaml": ""}, "pnpm install --frozen-lockfile"},
		{map[string]string{"yarn.lock": "# yarn lockfile v1\n"}, "yarn install --frozen-lockfile"},
		{map[string]string{"yarn.lock": "__metadata:\n", ".yarnrc.yml": "nodeLinker: node-modules\n"}, "yarn install --immutable"},
		{map[string]string{"bun.lockb": ""}, "bun install --frozen-lockfile"},
		{map[string]string{"package-lock.json": "{}"}, "npm ci"},
		{nil, "npm install"},
	}
	for _, c := range cases {
		t.Run(c.want, func(t *testing.T) {
			isolateInstallMarkers(t)
			log := fakeTools(t, fakeInstallerBody, "npm", "pnpm", "yarn", "bun")
			web := t.TempDir()
			writeTestFile(t, filepath.Join(web, "package.json"), "{}")
			for name, body := range c.files {
				writeTestFile(t, filepath.Join(web, name), body)
			}
			var out strings.Builder
			if err := ensureNodeModules(context.Background(), web, &out, &out); err != nil {
				t.Fatalf("ensureNodeModules: %v\n%s", err, out.String())
			}
			if got := installCalls(t, log); len(got) != 1 || got[0] != c.want {
				t.Errorf("ran %q, want %q", got, c.want)
			}
			if !strings.Contains(out.String(), c.want) {
				t.Errorf("output does not name the install: %q", out.String())
			}
		})
	}
}

// Cancelling an install (SIGTERM to nexus) must stop the package
// manager's children too, and the next run must install again even though
// the interrupted one had already put a Vite in place.
func TestEnsureNodeModules_InterruptedInstallIsStoppedAndRetried(t *testing.T) {
	isolateInstallMarkers(t)
	// The fake npm installs Vite first, then "keeps downloading" in a
	// child it waits for — the state a real interrupted install leaves.
	log := fakeTools(t, fakeInstallerBody+`if [ ! -f .second ]; then
  touch .second
  sleep 30 &
  echo $! > child.pid
  wait
fi
`, "npm")
	web := t.TempDir()
	writeTestFile(t, filepath.Join(web, "package.json"), "{}")

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- ensureNodeModules(ctx, web, &strings.Builder{}, &strings.Builder{}) }()
	pidFile := filepath.Join(web, "child.pid")
	for deadline := time.Now().Add(5 * time.Second); !fileExists(pidFile) && time.Now().Before(deadline); {
		time.Sleep(20 * time.Millisecond)
	}
	cancel()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "interrupted") {
			t.Errorf("err = %v, want interrupted", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("ensureNodeModules did not return after cancel")
	}
	b, _ := os.ReadFile(pidFile)
	pid, _ := strconv.Atoi(strings.TrimSpace(string(b)))
	if pid <= 0 {
		t.Fatal("fake npm never started its child")
	}
	for i := 0; i < 50 && syscall.Kill(pid, 0) == nil; i++ {
		time.Sleep(20 * time.Millisecond)
	}
	if syscall.Kill(pid, 0) == nil {
		_ = syscall.Kill(pid, syscall.SIGKILL)
		t.Error("the installer's child survived the cancel")
	}
	if _, ok := viteBinary(web); !ok {
		t.Fatal("fixture: the interrupted install should have left a Vite behind")
	}

	var out strings.Builder
	if err := ensureNodeModules(context.Background(), web, &out, &out); err != nil {
		t.Fatalf("retry: %v", err)
	}
	if got := installCalls(t, log); len(got) != 2 {
		t.Errorf("installs = %q, want the interrupted one retried", got)
	}
	if !strings.Contains(out.String(), "did not finish") {
		t.Errorf("retry not explained: %q", out.String())
	}
	if err := ensureNodeModules(context.Background(), web, &out, &out); err != nil {
		t.Fatal(err)
	}
	if got := installCalls(t, log); len(got) != 2 {
		t.Errorf("installs = %q, want none once an install finished", got)
	}
}
