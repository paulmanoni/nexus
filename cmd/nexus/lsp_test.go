package main

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// lspClient drives a proxy the way an editor does.
type lspClient struct {
	t     *testing.T
	conn  *rpcConn
	mu    sync.Mutex
	id    int
	wait  map[string]chan json.RawMessage
	diags chan diagEvent
	seen  map[string][]lspDiagnostic // latest diagnostics by path
}

type diagEvent struct {
	path  string
	diags []lspDiagnostic
}

func (c *lspClient) loop() {
	for {
		body, err := c.conn.read()
		if err != nil {
			return
		}
		var m rpcMsg
		if json.Unmarshal(body, &m) != nil {
			continue
		}
		switch {
		case m.isRequest():
			_ = c.conn.send(map[string]any{"jsonrpc": "2.0", "id": m.ID, "result": nil})
		case m.Method == "textDocument/publishDiagnostics":
			var p struct {
				URI         string          `json:"uri"`
				Diagnostics []lspDiagnostic `json:"diagnostics"`
			}
			_ = json.Unmarshal(m.Params, &p)
			c.diags <- diagEvent{uriToPath(p.URI), p.Diagnostics}
		case m.isResponse():
			c.mu.Lock()
			ch := c.wait[string(m.ID)]
			c.mu.Unlock()
			if ch != nil {
				res := m.Result
				if len(m.Error) > 0 {
					res = m.Error
				}
				ch <- res
			}
		}
	}
}

func (c *lspClient) call(method string, params any) json.RawMessage {
	c.t.Helper()
	c.mu.Lock()
	c.id++
	id := c.id
	ch := make(chan json.RawMessage, 1)
	c.wait[jsonInt(id)] = ch
	c.mu.Unlock()
	_ = c.conn.send(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params})
	select {
	case r := <-ch:
		return r
	case <-time.After(60 * time.Second):
		c.t.Fatalf("%s: no response", method)
		return nil
	}
}

func jsonInt(n int) string { b, _ := json.Marshal(n); return string(b) }

func (c *lspClient) notify(method string, params any) {
	_ = c.conn.send(map[string]any{"jsonrpc": "2.0", "method": method, "params": params})
}

// waitDiags waits until ok accepts the diagnostics published for path.
func (c *lspClient) waitDiags(path string, ok func([]lspDiagnostic) bool) []lspDiagnostic {
	c.t.Helper()
	deadline := time.After(60 * time.Second)
	last, ok0 := c.seen[path]
	if ok0 && ok(last) {
		return last
	}
	for {
		select {
		case ev := <-c.diags:
			c.seen[ev.path] = ev.diags
			if ev.path != path {
				continue
			}
			last = ev.diags
			if ok(ev.diags) {
				return ev.diags
			}
		case <-deadline:
			c.t.Fatalf("diagnostics for %s never matched; last: %+v", path, last)
		}
	}
}

func (c *lspClient) open(path, text string) {
	lang := "go"
	if strings.HasSuffix(path, ".templ") {
		lang = "templ"
	}
	c.notify("textDocument/didOpen", map[string]any{"textDocument": map[string]any{
		"uri": pathToURI(path), "languageId": lang, "version": 1, "text": text}})
}

func at(path string, line, char int) map[string]any {
	return map[string]any{"textDocument": map[string]any{"uri": pathToURI(path)},
		"position": map[string]any{"line": line, "character": char}}
}

// lspFixture is a module whose views exist only as .templ: no generated Go
// on disk.
func lspFixture(t *testing.T) (gopls, root string) {
	t.Helper()
	gopls, err := findGopls()
	if err != nil {
		t.Skip(err)
	}
	root, _ = filepath.EvalSymlinks(t.TempDir())
	files := map[string]string{
		"go.mod":            "module example.com/app\n\ngo 1.26\n\nrequire github.com/a-h/templ v0.3.1020\n",
		"views/hello.templ": helloTempl,
		"app/app.go":        appGo,
		"api/api.go":        "package api\n\n//nexus:rset GET /pets\nfunc ListPets() {}\n",
	}
	for name, src := range files {
		path := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(src), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// The generated code imports templ: record its checksums from the
	// module cache (offline).
	dl := exec.Command("go", "mod", "download", "github.com/a-h/templ")
	dl.Dir = root
	dl.Env = append(os.Environ(), "GOWORK=off", "GOFLAGS=-mod=mod", "GOPROXY=off")
	if out, err := dl.CombinedOutput(); err != nil {
		t.Skipf("templ is not in the module cache: %v\n%s", err, out)
	}
	return gopls, root
}

const helloTempl = `package views

templ Hello(name string) {
	<p>Hello, { name }</p>
}
`

const appGo = `package app

import (
	"context"
	"io"

	"example.com/app/views"
)

func Render(w io.Writer) error {
	return views.Hello("pets").Render(context.Background(), w)
}
`

func startLSP(t *testing.T, gopls, root string) *lspClient {
	t.Helper()
	t.Chdir(root)
	t.Setenv("GOWORK", "off")
	t.Setenv("GOFLAGS", "-mod=mod")
	t.Setenv("GOPROXY", "off")
	edIn, proxyOut := io.Pipe()
	proxyIn, edOut := io.Pipe()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		_ = runLSP(ctx, proxyIn, proxyOut, io.Discard, gopls)
		close(done)
	}()
	c := &lspClient{t: t, conn: newRPCConn(edIn, edOut), wait: map[string]chan json.RawMessage{}, diags: make(chan diagEvent, 256), seen: map[string][]lspDiagnostic{}}
	go c.loop()
	t.Cleanup(func() {
		c.call("shutdown", nil)
		c.notify("exit", nil)
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			cancel()
			<-done
		}
		cancel()
		edOut.Close()
	})
	c.call("initialize", map[string]any{"processId": os.Getpid(), "rootUri": pathToURI(root),
		"capabilities": map[string]any{}, "workspaceFolders": []any{map[string]any{"uri": pathToURI(root), "name": "app"}}})
	c.notify("initialized", map[string]any{})
	return c
}

func TestLSPProxyGeneratedViews(t *testing.T) {
	gopls, root := lspFixture(t)
	c := startLSP(t, gopls, root)
	app := filepath.Join(root, "app", "app.go")
	templ := filepath.Join(root, "views", "hello.templ")

	if _, err := os.Stat(filepath.Join(root, "views", "hello_templ.go")); err == nil {
		t.Fatal("the fixture must not have generated Go on disk")
	}

	// Go calling a component compiles against the generated buffer.
	c.open(app, appGo)
	c.waitDiags(app, func(ds []lspDiagnostic) bool { return len(ds) == 0 })

	// A malformed //nexus: directive is a diagnostic on its file.
	ds := c.waitDiags(filepath.Join(root, "api", "api.go"), func(ds []lspDiagnostic) bool { return len(ds) > 0 })
	if !strings.Contains(ds[0].Message, "unknown annotation //nexus:rset") || ds[0].Range.Start.Line != 2 {
		t.Fatalf("directive diagnostic = %+v", ds[0])
	}

	// Definition of views.Hello lands on the .templ, not the generated Go.
	var locs []struct {
		URI   string   `json:"uri"`
		Range lspRange `json:"range"`
	}
	raw := c.call("textDocument/definition", at(app, 10, 16))
	if err := json.Unmarshal(raw, &locs); err != nil || len(locs) != 1 {
		t.Fatalf("definition = %s", raw)
	}
	if uriToPath(locs[0].URI) != templ || locs[0].Range.Start.Line != 2 {
		t.Fatalf("definition = %s, want %s line 2", raw, templ)
	}

	// In the .templ: hover on a Go expression answers through gopls.
	c.open(templ, helloTempl)
	raw = c.call("textDocument/hover", at(templ, 3, 15))
	if !strings.Contains(string(raw), "name string") {
		t.Fatalf("hover on { name } = %s", raw)
	}
	// Definition from the .templ expression stays in the .templ.
	raw = c.call("textDocument/definition", at(templ, 3, 15))
	if err := json.Unmarshal(raw, &locs); err != nil || len(locs) != 1 || uriToPath(locs[0].URI) != templ || locs[0].Range.Start.Line != 2 {
		t.Fatalf("definition of name = %s", raw)
	}

	// A Go type error inside the .templ is reported on the .templ.
	bad := strings.Replace(helloTempl, "{ name }", "{ name + 1 }", 1)
	c.notify("textDocument/didChange", map[string]any{"textDocument": map[string]any{"uri": pathToURI(templ), "version": 2},
		"contentChanges": []any{map[string]any{"text": bad}}})
	ds = c.waitDiags(templ, func(ds []lspDiagnostic) bool { return len(ds) > 0 })
	if ds[0].Range.Start.Line != 3 {
		t.Fatalf("type error at %+v, want line 3: %s", ds[0].Range, ds[0].Message)
	}

	// A templ syntax error comes from the views compiler, and the Go side
	// keeps the last good generated code.
	broken := strings.Replace(helloTempl, "{ name }", "{ ", 1)
	c.notify("textDocument/didChange", map[string]any{"textDocument": map[string]any{"uri": pathToURI(templ), "version": 3},
		"contentChanges": []any{map[string]any{"text": broken}}})
	ds = c.waitDiags(templ, func(ds []lspDiagnostic) bool { return len(ds) > 0 && ds[0].Source == "nexus" })
	if ds[0].Range.Start.Line != 3 {
		t.Fatalf("syntax error at %+v: %s", ds[0].Range, ds[0].Message)
	}

	// Renaming the component breaks the Go caller.
	renamed := strings.Replace(helloTempl, "templ Hello(", "templ Greet(", 1)
	c.notify("textDocument/didChange", map[string]any{"textDocument": map[string]any{"uri": pathToURI(templ), "version": 4},
		"contentChanges": []any{map[string]any{"text": renamed}}})
	ds = c.waitDiags(app, func(ds []lspDiagnostic) bool { return len(ds) > 0 })
	if !strings.Contains(ds[0].Message, "Hello") {
		t.Fatalf("app diagnostic = %+v", ds)
	}
}

func TestOffsetOfUTF16(t *testing.T) {
	buf := []byte("ab\nx😀y\n")
	if got := offsetOf(buf, lspPosition{Line: 1, Character: 3}); string(buf[got:got+1]) != "y" {
		t.Fatalf("offset = %d", got)
	}
	if got := offsetOf(buf, lspPosition{Line: 5}); got != len(buf) {
		t.Fatalf("past the end = %d", got)
	}
}

func TestDiagnosticsFrom(t *testing.T) {
	t.Chdir(t.TempDir())
	wd, _ := os.Getwd()
	err := errorString("scan: api/api.go:3: unknown annotation //nexus:rset\nviews/x.templ:4:7: bad\n  more detail")
	got := diagnosticsFrom(err)
	if d := got[filepath.Join(wd, "api", "api.go")]; len(d) != 1 || d[0].Range.Start.Line != 2 {
		t.Fatalf("go diag = %+v", got)
	}
	d := got[filepath.Join(wd, "views", "x.templ")]
	if len(d) != 1 || d[0].Range.Start != (lspPosition{3, 6}) || !strings.HasSuffix(d[0].Message, "more detail") {
		t.Fatalf("templ diag = %+v", d)
	}
}

type errorString string

func (e errorString) Error() string { return string(e) }
