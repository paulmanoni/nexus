package main

import (
	"bufio"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/paulmanoni/nexus/frontend/deps/lockfile"
	"github.com/paulmanoni/nexus/frontend/deps/store"
)

// esmProject scaffolds a minimal JS-only frontend (no .vue, so no SFC
// compiler bootstrap is needed) with a warm cache + lockfile entry for a
// single fake "vue" dependency, then returns the project root.
func esmProject(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	srcDir := filepath.Join(root, "islands.src")
	if err := os.MkdirAll(srcDir, 0o755); err != nil {
		t.Fatal(err)
	}
	write := func(rel, body string) {
		p := filepath.Join(srcDir, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("index.html", `<html><head></head><body><div id="app"></div><script type="module" src="/main.js"></script></body></html>`)
	write("main.js", `import { createApp } from "vue";`+"\n"+`import App from "./App.js";`+"\n"+`createApp(App).mount("#app");`)
	write("App.js", `export default { name: "App" };`)

	// Warm cache + lockfile with one cached "vue".
	cacheDir := filepath.Join(root, ".cache")
	st, err := store.New(cacheDir)
	if err != nil {
		t.Fatal(err)
	}
	const vueURL = "https://esm.sh/vue@3.5.13/es2022/vue.mjs"
	if _, err := st.Put(vueURL, strings.NewReader(`export const createApp = () => ({ mount() {} });`), "", store.Metadata{ContentType: "application/javascript"}); err != nil {
		t.Fatal(err)
	}
	lf := lockfile.New()
	lf.Add(lockfile.Package{Spec: "vue", Version: "3.5.13", Resolved: vueURL})
	if err := lf.Save(filepath.Join(root, lockfile.Filename)); err != nil {
		t.Fatal(err)
	}
	t.Setenv("NEXUS_CACHE", cacheDir)
	return root
}

// waitForURL blocks until ch yields a dev URL or the deadline elapses.
func waitForURL(t *testing.T, ch <-chan string) string {
	t.Helper()
	select {
	case u := <-ch:
		return u
	case <-time.After(10 * time.Second):
		t.Fatal("startESMWatcher never reported a dev URL")
		return ""
	}
}

func TestStartESMWatcher_ServesAndProxies(t *testing.T) {
	root := esmProject(t)

	// Stub backend the dev server should proxy unserved paths to.
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("API:" + r.URL.Path))
	}))
	defer backend.Close()
	appAddr := strings.TrimPrefix(backend.URL, "http://") // host:port

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	urlCh := make(chan string, 1)
	if err := startESMWatcher(ctx, root, appAddr, false, io.Discard, io.Discard, urlCh); err != nil {
		t.Fatalf("startESMWatcher: %v", err)
	}
	base := strings.TrimSuffix(waitForURL(t, urlCh), "/")

	get := func(p string) (int, string) {
		resp, err := http.Get(base + p)
		if err != nil {
			t.Fatalf("GET %s: %v", p, err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}

	// index.html: HMR client injected, ahead of the entry module.
	code, body := get("/")
	if code != 200 || !strings.Contains(body, "/@viteless/client.js") {
		t.Errorf("index.html missing client (code %d):\n%s", code, body)
	}
	if strings.Index(body, "/@viteless/client.js") > strings.Index(body, "/main.js") {
		t.Errorf("client script must precede entry module:\n%s", body)
	}

	// main.js: bare "vue" rewritten to a /@dep/ URL, relative kept in tree.
	code, body = get("/main.js")
	if code != 200 {
		t.Fatalf("main.js status %d", code)
	}
	// vue rewrites to a /@dep/ URL. devVueRewrite swaps in the dev build
	// (vue.development.mjs) when esm.sh is reachable and falls back to the
	// plain build offline — accept either by matching the shared prefix.
	if !strings.Contains(body, `from "/@dep/https/esm.sh/vue@3.5.13/es2022/vue`) {
		t.Errorf("bare vue not rewritten to dep URL:\n%s", body)
	}
	if !strings.Contains(body, `from "/App.js"`) {
		t.Errorf("relative ./App.js not rewritten:\n%s", body)
	}
	if !strings.Contains(body, `import.meta.hot = __viteless_hot("/main.js")`) {
		t.Errorf("main.js missing hot preamble:\n%s", body)
	}

	// The dep blob itself is served under /@dep/.
	code, body = get("/@dep/https/esm.sh/vue@3.5.13/es2022/vue.mjs")
	if code != 200 || !strings.Contains(body, "createApp") {
		t.Errorf("dep blob not served (code %d):\n%s", code, body)
	}

	// Unserved path proxies to the backend.
	code, body = get("/graphql")
	if code != 200 || body != "API:/graphql" {
		t.Errorf("proxy fallback failed: %d %q", code, body)
	}
}

func TestStartESMWatcher_HMRBroadcastOnEdit(t *testing.T) {
	root := esmProject(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	urlCh := make(chan string, 1)
	if err := startESMWatcher(ctx, root, "127.0.0.1:0", false, io.Discard, io.Discard, urlCh); err != nil {
		t.Fatalf("startESMWatcher: %v", err)
	}
	base := strings.TrimSuffix(waitForURL(t, urlCh), "/")

	resp, err := http.Get(base + "/@viteless/hmr")
	if err != nil {
		t.Fatalf("SSE connect: %v", err)
	}
	defer resp.Body.Close()

	lines := make(chan string, 16)
	go func() {
		sc := bufio.NewScanner(resp.Body)
		for sc.Scan() {
			lines <- sc.Text()
		}
	}()

	// Drain the ": connected" preamble so the client is registered.
	select {
	case <-lines:
	case <-time.After(3 * time.Second):
		t.Fatal("no SSE preamble")
	}

	mainJS := filepath.Join(root, "islands.src", "main.js")
	deadline := time.After(5 * time.Second)
	tick := time.NewTicker(150 * time.Millisecond)
	defer tick.Stop()
	for {
		// Re-touch on each tick — fsnotify can miss the first write while
		// the watcher is still arming.
		_ = os.WriteFile(mainJS, []byte(`console.log("edited", Math.random());`), 0o644)
		select {
		case l := <-lines:
			if strings.Contains(l, `"type":"update"`) && strings.Contains(l, `"path":"/main.js"`) {
				return // success
			}
		case <-tick.C:
		case <-deadline:
			t.Fatal("no HMR update broadcast after editing main.js")
		}
	}
}
