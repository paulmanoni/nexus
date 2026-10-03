package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"go/parser"
	"go/token"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf16"
	"unicode/utf8"

	"github.com/spf13/cobra"

	"github.com/paulmanoni/nexus/v2/view/viewgen"
)

func newLSPCmd(stdout, stderr io.Writer) *cobra.Command {
	var goplsPath, logPath string
	cmd := &cobra.Command{
		Use:   "lsp",
		Short: "Language server for .go and .templ files (gopls plus nexus codegen)",
		Long: `Serve the Language Server Protocol over stdin/stdout, for .go and .templ files.

nexus lsp sits in front of gopls. It compiles the project's views (.templ) and
//nexus: handler registrations in memory and hands the generated Go to gopls
as open editor buffers, so Go code that calls a templ component type-checks,
completes and jumps to definition without *_templ.go, view_gen.go or
nexus_handlers_gen.go on disk.

For .templ files it reports the views compiler's errors and the Go type errors
of the expressions inside them, and answers definition, hover, completion and
references through gopls at the matching position of the generated code.
Malformed //nexus: directives show up as diagnostics on the .go file.

Point your editor's Go and templ language server at "nexus lsp" (it starts
gopls itself; --gopls picks the binary).`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if goplsPath == "" {
				p, err := findGopls()
				if err != nil {
					return err
				}
				goplsPath = p
			}
			logw := io.Discard
			if logPath != "" {
				f, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
				if err != nil {
					return err
				}
				defer f.Close()
				logw = f
			}
			ctx := cmd.Context()
			if ctx == nil {
				ctx = context.Background()
			}
			return runLSP(ctx, os.Stdin, stdout, logw, goplsPath)
		},
	}
	cmd.Flags().StringVar(&goplsPath, "gopls", "", "gopls binary (default: gopls on PATH, then $GOPATH/bin/gopls)")
	cmd.Flags().StringVar(&logPath, "log", "", "append a debug log to this file")
	return cmd
}

// findGopls locates gopls: PATH first, then GOBIN / GOPATH/bin.
func findGopls() (string, error) {
	if p, err := exec.LookPath("gopls"); err == nil {
		return p, nil
	}
	for _, key := range []string{"GOBIN", "GOPATH"} {
		out, err := exec.Command("go", "env", key).Output()
		if err != nil || strings.TrimSpace(string(out)) == "" {
			continue
		}
		dir := strings.TrimSpace(string(out))
		if key == "GOPATH" {
			dir = filepath.Join(filepath.SplitList(dir)[0], "bin")
		}
		if p := filepath.Join(dir, "gopls"); fileExists(p) {
			return p, nil
		}
	}
	return "", errors.New("gopls not found — install it with `go install golang.org/x/tools/gopls@latest` or pass --gopls")
}

// runLSP serves one editor session on in/out, with gopls as a child.
func runLSP(ctx context.Context, in io.Reader, out io.Writer, logw io.Writer, gopls string) error {
	child := exec.Command(gopls, "serve")
	child.Stderr = logw
	cin, err := child.StdinPipe()
	if err != nil {
		return err
	}
	cout, err := child.StdoutPipe()
	if err != nil {
		return err
	}
	if err := child.Start(); err != nil {
		return fmt.Errorf("start gopls: %w", err)
	}
	p := newLSPProxy(newRPCConn(in, out), newRPCConn(cout, cin), logw)
	goplsDone := make(chan struct{})
	go func() {
		p.fromGopls()
		close(goplsDone)
	}()
	editorDone := make(chan struct{})
	go func() {
		p.fromEditor()
		close(editorDone)
	}()
	select {
	case <-editorDone:
	case <-goplsDone:
	case <-ctx.Done():
	}
	p.stop()
	_ = cin.Close()
	exited := make(chan error, 1)
	go func() { exited <- child.Wait() }()
	select {
	case <-exited:
	case <-time.After(2 * time.Second):
		_ = child.Process.Kill()
		<-exited
	}
	return nil
}

// virtualFile is generated Go the proxy keeps open in gopls.
type virtualFile struct {
	content []byte
	version int
	views   bool // from the views compiler (else handler codegen)
}

// templRequest is an editor request on a .templ file, answered by gopls
// at the matching position of the generated Go.
type templRequest struct {
	editorID json.RawMessage
	method   string
	m        *viewgen.TemplMap
}

// lspProxy relays an editor and gopls, adding generated files as buffers.
type lspProxy struct {
	editor, gopls *rpcConn
	log           io.Writer

	genMu sync.Mutex // one regeneration at a time

	mu         sync.Mutex
	root       string
	nextID     int
	pending    map[string]string        // editor request id → method (location results to rewrite)
	templReqs  map[string]*templRequest // proxy request id → templ request
	templBufs  map[string][]byte        // .templ path → editor buffer
	virtual    map[string]*virtualFile  // generated path → buffer in gopls
	maps       map[string]*viewgen.TemplMap
	viewDiags  map[string][]lspDiagnostic // .templ path → views compiler errors
	goDiags    map[string][]any           // .templ path → gopls diagnostics mapped from its Go
	nexusDiags map[string][]lspDiagnostic // .go path → codegen errors
	goplsDiags map[string][]any           // .go path → gopls's latest diagnostics
	started    bool
	dirty      bool
	timer      *time.Timer
	stopped    bool
}

func newLSPProxy(editor, gopls *rpcConn, log io.Writer) *lspProxy {
	if log == nil {
		log = io.Discard
	}
	return &lspProxy{
		editor: editor, gopls: gopls, log: log,
		pending: map[string]string{}, templReqs: map[string]*templRequest{},
		templBufs: map[string][]byte{}, virtual: map[string]*virtualFile{},
		maps: map[string]*viewgen.TemplMap{}, viewDiags: map[string][]lspDiagnostic{},
		goDiags: map[string][]any{}, nexusDiags: map[string][]lspDiagnostic{},
		goplsDiags: map[string][]any{},
	}
}

func (p *lspProxy) logf(format string, args ...any) {
	fmt.Fprintf(p.log, "nexus lsp: "+format+"\n", args...)
}

func (p *lspProxy) stop() {
	p.mu.Lock()
	p.stopped = true
	if p.timer != nil {
		p.timer.Stop()
	}
	p.mu.Unlock()
}

// locationMethods return Location / LocationLink results that may point
// into generated files.
var locationMethods = map[string]bool{
	"textDocument/definition":     true,
	"textDocument/declaration":    true,
	"textDocument/typeDefinition": true,
	"textDocument/implementation": true,
	"textDocument/references":     true,
}

// templMethods are the .templ requests answered through gopls.
var templMethods = map[string]bool{
	"textDocument/definition":     true,
	"textDocument/declaration":    true,
	"textDocument/typeDefinition": true,
	"textDocument/implementation": true,
	"textDocument/references":     true,
	"textDocument/hover":          true,
	"textDocument/completion":     true,
	"textDocument/signatureHelp":  true,
}

func (p *lspProxy) fromEditor() {
	for {
		body, err := p.editor.read()
		if err != nil {
			return
		}
		var m rpcMsg
		if err := json.Unmarshal(body, &m); err != nil {
			p.logf("bad message from editor: %v", err)
			continue
		}
		if m.isResponse() {
			_ = p.gopls.write(body)
			continue
		}
		var doc struct {
			TextDocument textDocumentID `json:"textDocument"`
		}
		_ = json.Unmarshal(m.Params, &doc)
		path := uriToPath(doc.TextDocument.URI)
		templ := strings.HasSuffix(path, ".templ")

		switch m.Method {
		case "initialize":
			p.initialize(m.Params)
		case "initialized":
			_ = p.gopls.write(body)
			p.regenerate()
			p.mu.Lock()
			p.started = true
			p.mu.Unlock()
			continue
		case "exit":
			_ = p.gopls.write(body)
			return
		case "textDocument/didOpen", "textDocument/didChange", "textDocument/didClose", "textDocument/didSave":
			if templ {
				p.templDocument(m.Method, m.Params, path)
				continue
			}
			if p.isVirtual(path) {
				continue // the generated buffer stays the proxy's
			}
			_ = p.gopls.write(body)
			if m.Method == "textDocument/didSave" && strings.HasSuffix(path, ".go") {
				p.schedule()
			}
			continue
		case "workspace/didChangeWatchedFiles":
			_ = p.gopls.write(body)
			var params struct {
				Changes []struct {
					URI string `json:"uri"`
				} `json:"changes"`
			}
			_ = json.Unmarshal(m.Params, &params)
			for _, c := range params.Changes {
				if path := uriToPath(c.URI); (strings.HasSuffix(path, ".go") || strings.HasSuffix(path, ".templ")) && !p.isVirtual(path) {
					p.schedule()
					break
				}
			}
			continue
		}
		if templ {
			if m.isRequest() {
				p.templRequest(&m, path)
			}
			continue
		}
		if m.isRequest() && locationMethods[m.Method] {
			p.mu.Lock()
			p.pending[string(m.ID)] = m.Method
			p.mu.Unlock()
		}
		_ = p.gopls.write(body)
	}
}

func (p *lspProxy) fromGopls() {
	for {
		body, err := p.gopls.read()
		if err != nil {
			return
		}
		var m rpcMsg
		if err := json.Unmarshal(body, &m); err != nil {
			p.logf("bad message from gopls: %v", err)
			continue
		}
		switch {
		case m.isResponse():
			id := string(m.ID)
			p.mu.Lock()
			tr := p.templReqs[id]
			delete(p.templReqs, id)
			_, rewrite := p.pending[id]
			delete(p.pending, id)
			p.mu.Unlock()
			if tr != nil {
				p.answerTempl(tr, &m)
				continue
			}
			if rewrite && len(m.Error) == 0 && p.mentionsVirtual(m.Result) {
				var v any
				if json.Unmarshal(m.Result, &v) == nil {
					res, _ := json.Marshal(p.rewriteLocations(v, nil))
					m.Result = res
					_ = p.editor.send(&m)
					continue
				}
			}
			_ = p.editor.write(body)
		case m.Method == "textDocument/publishDiagnostics":
			p.goplsPublished(m.Params)
		default:
			_ = p.editor.write(body)
		}
	}
}

// initialize takes the workspace root from the editor's initialize params.
func (p *lspProxy) initialize(params json.RawMessage) {
	var ip struct {
		RootURI          string `json:"rootUri"`
		RootPath         string `json:"rootPath"`
		WorkspaceFolders []struct {
			URI string `json:"uri"`
		} `json:"workspaceFolders"`
	}
	_ = json.Unmarshal(params, &ip)
	root := uriToPath(ip.RootURI)
	if root == "" && len(ip.WorkspaceFolders) > 0 {
		root = uriToPath(ip.WorkspaceFolders[0].URI)
	}
	if root == "" {
		root = ip.RootPath
	}
	if root == "" {
		root, _ = os.Getwd()
	}
	// Codegen error positions are relative to the working directory.
	if err := os.Chdir(root); err != nil {
		p.logf("chdir %s: %v", root, err)
	}
	p.mu.Lock()
	p.root = root
	p.mu.Unlock()
	p.logf("root %s", root)
}

func (p *lspProxy) isVirtual(path string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	_, ok := p.virtual[path]
	return ok
}

func (p *lspProxy) mentionsVirtual(raw json.RawMessage) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	for path := range p.virtual {
		if bytes.Contains(raw, []byte(pathToURI(path))) {
			return true
		}
	}
	return false
}

// schedule regenerates after a short quiet period.
func (p *lspProxy) schedule() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.stopped || !p.started {
		p.dirty = true
		return
	}
	p.dirty = true
	if p.timer != nil {
		p.timer.Stop()
	}
	p.timer = time.AfterFunc(150*time.Millisecond, p.regenerate)
}

// templDocument tracks the editor's .templ buffers.
func (p *lspProxy) templDocument(method string, params json.RawMessage, path string) {
	switch method {
	case "textDocument/didOpen":
		var dp struct {
			TextDocument struct {
				Text string `json:"text"`
			} `json:"textDocument"`
		}
		_ = json.Unmarshal(params, &dp)
		p.mu.Lock()
		p.templBufs[path] = []byte(dp.TextDocument.Text)
		p.mu.Unlock()
	case "textDocument/didChange":
		var dp struct {
			ContentChanges []struct {
				Range *lspRange `json:"range"`
				Text  string    `json:"text"`
			} `json:"contentChanges"`
		}
		_ = json.Unmarshal(params, &dp)
		p.mu.Lock()
		buf := p.templBufs[path]
		for _, c := range dp.ContentChanges {
			if c.Range == nil {
				buf = []byte(c.Text)
				continue
			}
			start, end := offsetOf(buf, c.Range.Start), offsetOf(buf, c.Range.End)
			if end < start {
				end = start
			}
			next := make([]byte, 0, len(buf)-(end-start)+len(c.Text))
			next = append(append(append(next, buf[:start]...), c.Text...), buf[end:]...)
			buf = next
		}
		p.templBufs[path] = buf
		p.mu.Unlock()
	case "textDocument/didClose":
		p.mu.Lock()
		delete(p.templBufs, path)
		p.mu.Unlock()
	}
	p.schedule()
}

// offsetOf converts an LSP position (UTF-16 columns) to a byte offset.
func offsetOf(buf []byte, pos lspPosition) int {
	off := 0
	for line := uint32(0); line < pos.Line; line++ {
		i := bytes.IndexByte(buf[off:], '\n')
		if i < 0 {
			return len(buf)
		}
		off += i + 1
	}
	units := uint32(0)
	for off < len(buf) && units < pos.Character && buf[off] != '\n' {
		r, size := utf8.DecodeRune(buf[off:])
		units += uint32(utf16.RuneLen(r))
		off += size
	}
	return off
}

// regenerate recompiles views and handler registrations and syncs gopls's
// buffers and the diagnostics with the result. A generator that fails
// keeps its previous files, so the rest of the code keeps type-checking.
func (p *lspProxy) regenerate() {
	p.genMu.Lock()
	defer p.genMu.Unlock()
	p.mu.Lock()
	if p.stopped {
		p.mu.Unlock()
		return
	}
	p.dirty = false
	root := p.root
	bufs := make(map[string][]byte, len(p.templBufs))
	for k, v := range p.templBufs {
		bufs[k] = v
	}
	p.mu.Unlock()

	want := map[string]*virtualFile{}
	var maps map[string]*viewgen.TemplMap
	viewsOK := true
	var genErrs []error
	if len(bufs) > 0 || viewgen.HasTemplates(root) {
		plan, err := viewgen.GenerateWith(root, viewgen.Options{Sources: bufs, Editor: true})
		if err != nil {
			viewsOK = false
			genErrs = append(genErrs, err)
		} else {
			for path, content := range plan.Files {
				want[path] = &virtualFile{content: content, views: true}
			}
			for _, path := range plan.Remove {
				if stub := packageStub(path); stub != nil {
					want[path] = &virtualFile{content: stub, views: true}
				}
			}
			maps = plan.Maps
		}
	}
	handlersOK := true
	if results, err := allHandlerArtifacts(root, handlerGenFileName); err != nil {
		handlersOK = false
		genErrs = append(genErrs, err)
	} else {
		for _, r := range results {
			path, _ := filepath.Abs(r.Path)
			want[path] = &virtualFile{content: r.Content}
		}
	}
	diags := diagnosticsFrom(errors.Join(genErrs...))

	type op struct {
		method string
		params any
	}
	var ops []op
	p.mu.Lock()
	for path, vf := range p.virtual {
		if (vf.views && !viewsOK) || (!vf.views && !handlersOK) {
			if _, taken := want[path]; !taken {
				want[path] = vf
			}
		}
	}
	if !viewsOK {
		maps = p.maps
	}
	for path, vf := range want {
		old := p.virtual[path]
		uri := pathToURI(path)
		switch {
		case old == nil:
			vf.version = 1
			ops = append(ops, op{"textDocument/didOpen", map[string]any{"textDocument": map[string]any{
				"uri": uri, "languageId": "go", "version": vf.version, "text": string(vf.content)}}})
		case !bytes.Equal(old.content, vf.content):
			vf.version = old.version + 1
			ops = append(ops, op{"textDocument/didChange", map[string]any{
				"textDocument":   map[string]any{"uri": uri, "version": vf.version},
				"contentChanges": []any{map[string]any{"text": string(vf.content)}}}})
		default:
			vf.version = old.version
		}
	}
	for path := range p.virtual {
		if _, ok := want[path]; !ok {
			ops = append(ops, op{"textDocument/didClose", map[string]any{"textDocument": map[string]any{"uri": pathToURI(path)}}})
		}
	}
	p.virtual, p.maps = want, maps

	templTouched, goTouched := map[string]bool{}, map[string]bool{}
	for path := range p.viewDiags {
		templTouched[path] = true
	}
	for path := range p.nexusDiags {
		goTouched[path] = true
	}
	p.viewDiags, p.nexusDiags = map[string][]lspDiagnostic{}, map[string][]lspDiagnostic{}
	for path, ds := range diags {
		if strings.HasSuffix(path, ".templ") {
			p.viewDiags[path] = ds
			templTouched[path] = true
		} else {
			p.nexusDiags[path] = ds
			goTouched[path] = true
		}
	}
	p.mu.Unlock()

	for _, o := range ops {
		_ = p.gopls.send(map[string]any{"jsonrpc": "2.0", "method": o.method, "params": o.params})
	}
	for path := range templTouched {
		p.publishTempl(path)
	}
	for path := range goTouched {
		p.publishGo(path)
	}
}

// packageStub returns a Go file holding only path's package clause — what
// hides a stale generated file on disk from gopls — or nil when path does
// not exist.
func packageStub(path string) []byte {
	f, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.PackageClauseOnly)
	if err != nil {
		return nil
	}
	return []byte("package " + f.Name.Name + "\n")
}

// errPosition finds "file.go:12:3: msg" / "file.templ:4: msg" in an error line.
var errPosition = regexp.MustCompile(`([^\s:]+\.(?:go|templ)):(\d+)(?::(\d+))?: (.*)$`)

// diagnosticsFrom turns a codegen error into diagnostics by file. Lines
// without a position continue the previous message.
func diagnosticsFrom(err error) map[string][]lspDiagnostic {
	out := map[string][]lspDiagnostic{}
	if err == nil {
		return out
	}
	var last *lspDiagnostic
	for _, line := range strings.Split(err.Error(), "\n") {
		m := errPosition.FindStringSubmatch(line)
		if m == nil {
			if last != nil && strings.TrimSpace(line) != "" {
				last.Message += "\n" + strings.TrimSpace(line)
			}
			continue
		}
		path, _ := filepath.Abs(m[1])
		ln, _ := strconv.Atoi(m[2])
		col := 1
		if m[3] != "" {
			col, _ = strconv.Atoi(m[3])
		}
		pos := lspPosition{Line: uint32(max(ln-1, 0)), Character: uint32(max(col-1, 0))}
		out[path] = append(out[path], lspDiagnostic{Range: lspRange{pos, pos}, Severity: 1, Source: "nexus", Message: m[4]})
		last = &out[path][len(out[path])-1]
	}
	return out
}

func (p *lspProxy) publishTempl(path string) {
	p.mu.Lock()
	ds := []any{}
	for _, d := range p.viewDiags[path] {
		ds = append(ds, d)
	}
	if len(p.viewDiags[path]) == 0 {
		ds = append(ds, p.goDiags[path]...)
	}
	p.mu.Unlock()
	p.publish(path, ds)
}

func (p *lspProxy) publishGo(path string) {
	p.mu.Lock()
	ds := append([]any{}, p.goplsDiags[path]...)
	for _, d := range p.nexusDiags[path] {
		ds = append(ds, d)
	}
	p.mu.Unlock()
	p.publish(path, ds)
}

func (p *lspProxy) publish(path string, ds []any) {
	_ = p.editor.send(map[string]any{"jsonrpc": "2.0", "method": "textDocument/publishDiagnostics",
		"params": map[string]any{"uri": pathToURI(path), "diagnostics": ds}})
}

// goplsPublished routes gopls's diagnostics: a generated file's onto its
// .templ (or nowhere), a real file's to the editor with nexus's added.
func (p *lspProxy) goplsPublished(params json.RawMessage) {
	var dp struct {
		URI         string `json:"uri"`
		Diagnostics []any  `json:"diagnostics"`
	}
	if err := json.Unmarshal(params, &dp); err != nil {
		return
	}
	path := uriToPath(dp.URI)
	p.mu.Lock()
	_, virtual := p.virtual[path]
	m := p.maps[path]
	if !virtual {
		p.goplsDiags[path] = dp.Diagnostics
		p.mu.Unlock()
		p.publishGo(path)
		return
	}
	if m == nil {
		p.mu.Unlock()
		return
	}
	var mapped []any
	for _, d := range dp.Diagnostics {
		obj, ok := d.(map[string]any)
		if !ok {
			continue
		}
		r, ok := mapRange(m, obj["range"])
		if !ok {
			continue
		}
		obj["range"] = r
		delete(obj, "relatedInformation")
		delete(obj, "data")
		mapped = append(mapped, obj)
	}
	p.goDiags[m.Templ] = mapped
	p.mu.Unlock()
	p.publishTempl(m.Templ)
}

// toRange decodes a JSON range.
func toRange(v any) (lspRange, bool) {
	b, err := json.Marshal(v)
	if err != nil {
		return lspRange{}, false
	}
	var r lspRange
	return r, json.Unmarshal(b, &r) == nil
}

// mapRange maps a range in generated Go back to the .templ.
func mapRange(m *viewgen.TemplMap, v any) (lspRange, bool) {
	r, ok := toRange(v)
	if !ok {
		return r, false
	}
	start, ok := m.Map.SourcePositionFromTarget(r.Start.Line, r.Start.Character)
	if !ok {
		sym, ok := m.Map.SymbolSourceRangeFromTarget(r.Start.Line, r.Start.Character)
		if !ok {
			return r, false
		}
		return lspRange{lspPosition{sym.From.Line, sym.From.Col}, lspPosition{sym.To.Line, sym.To.Col}}, true
	}
	// Expressions are copied verbatim, so a one-line range keeps its width.
	out := lspRange{Start: lspPosition{start.Line, start.Col}, End: lspPosition{start.Line, start.Col}}
	if r.End.Line == r.Start.Line && r.End.Character > r.Start.Character {
		out.End.Character = start.Col + (r.End.Character - r.Start.Character)
	}
	return out, true
}

// rewriteLocations maps Location / LocationLink values pointing into
// generated files: a *_templ.go onto its .templ, anything else dropped (it
// has no file the editor could open). origin maps originSelectionRange
// when the request came from a .templ.
func (p *lspProxy) rewriteLocations(v any, origin *viewgen.TemplMap) any {
	switch x := v.(type) {
	case []any:
		out := make([]any, 0, len(x))
		for _, e := range x {
			if r := p.rewriteLocations(e, origin); r != nil {
				out = append(out, r)
			}
		}
		return out
	case map[string]any:
		if uri, ok := x["uri"].(string); ok {
			nu, nr, keep := p.mapLocation(uri, x["range"])
			if !keep {
				return nil
			}
			x["uri"], x["range"] = nu, nr
			return x
		}
		if uri, ok := x["targetUri"].(string); ok {
			nu, nr, keep := p.mapLocation(uri, x["targetRange"])
			if !keep {
				return nil
			}
			_, sr, _ := p.mapLocation(uri, x["targetSelectionRange"])
			x["targetUri"], x["targetRange"], x["targetSelectionRange"] = nu, nr, sr
			if origin != nil {
				if r, ok := mapRange(origin, x["originSelectionRange"]); ok {
					x["originSelectionRange"] = r
				} else {
					delete(x, "originSelectionRange")
				}
			}
			return x
		}
	}
	return v
}

func (p *lspProxy) mapLocation(uri string, rng any) (string, any, bool) {
	path := uriToPath(uri)
	p.mu.Lock()
	_, virtual := p.virtual[path]
	m := p.maps[path]
	p.mu.Unlock()
	if !virtual {
		return uri, rng, true
	}
	if m == nil {
		return "", nil, false
	}
	if r, ok := mapRange(m, rng); ok {
		return pathToURI(m.Templ), r, true
	}
	return pathToURI(m.Templ), lspRange{}, true
}

// templRequest answers a .templ request through gopls at the generated
// Go's matching position, or with null where there is none.
func (p *lspProxy) templRequest(m *rpcMsg, path string) {
	if !templMethods[m.Method] {
		_ = p.editor.send(map[string]any{"jsonrpc": "2.0", "id": m.ID, "result": nil})
		return
	}
	p.mu.Lock()
	dirty := p.dirty
	p.mu.Unlock()
	if dirty {
		p.regenerate()
	}
	var params map[string]any
	_ = json.Unmarshal(m.Params, &params)
	var tp struct {
		Position lspPosition `json:"position"`
	}
	_ = json.Unmarshal(m.Params, &tp)

	p.mu.Lock()
	var gen string
	var tm *viewgen.TemplMap
	for g, mm := range p.maps {
		if mm.Templ == path {
			gen, tm = g, mm
			break
		}
	}
	var tgt struct{ Line, Col uint32 }
	ok := tm != nil
	if ok {
		pos, found := tm.Map.TargetPositionFromSource(tp.Position.Line, tp.Position.Character)
		tgt.Line, tgt.Col, ok = pos.Line, pos.Col, found
	}
	if !ok {
		p.mu.Unlock()
		_ = p.editor.send(map[string]any{"jsonrpc": "2.0", "id": m.ID, "result": nil})
		return
	}
	p.nextID++
	id := fmt.Sprintf("%q", "nexus-lsp:"+strconv.Itoa(p.nextID))
	p.templReqs[id] = &templRequest{editorID: m.ID, method: m.Method, m: tm}
	p.mu.Unlock()

	params["textDocument"] = map[string]any{"uri": pathToURI(gen)}
	params["position"] = map[string]any{"line": tgt.Line, "character": tgt.Col}
	_ = p.gopls.send(map[string]any{"jsonrpc": "2.0", "id": json.RawMessage(id), "method": m.Method, "params": params})
}

// answerTempl translates gopls's answer for a .templ request.
func (p *lspProxy) answerTempl(tr *templRequest, m *rpcMsg) {
	reply := map[string]any{"jsonrpc": "2.0", "id": tr.editorID}
	if len(m.Error) > 0 {
		reply["error"] = m.Error
		_ = p.editor.send(reply)
		return
	}
	var v any
	_ = json.Unmarshal(m.Result, &v)
	switch {
	case v == nil:
	case locationMethods[tr.method]:
		v = p.rewriteLocations(v, tr.m)
	case tr.method == "textDocument/hover":
		if h, ok := v.(map[string]any); ok {
			if r, ok := mapRange(tr.m, h["range"]); ok {
				h["range"] = r
			} else {
				delete(h, "range")
			}
		}
	case tr.method == "textDocument/completion":
		items := v
		if list, ok := v.(map[string]any); ok {
			items = list["items"]
			delete(list, "itemDefaults")
		}
		if arr, ok := items.([]any); ok {
			for _, it := range arr {
				if item, ok := it.(map[string]any); ok {
					completionItemToTempl(tr.m, item)
				}
			}
		}
	}
	reply["result"] = v
	_ = p.editor.send(reply)
}

// completionItemToTempl maps an item's edit range onto the .templ, or
// falls back to plain insert text when it does not map.
func completionItemToTempl(m *viewgen.TemplMap, item map[string]any) {
	delete(item, "additionalTextEdits") // imports into the generated file
	te, ok := item["textEdit"].(map[string]any)
	if !ok {
		return
	}
	key := "range"
	if _, ok := te["range"]; !ok {
		key = "replace"
		delete(te, "insert")
	}
	if r, ok := mapRange(m, te[key]); ok {
		te["range"] = r
		delete(te, "replace")
		return
	}
	if text, ok := te["newText"].(string); ok {
		item["insertText"] = text
	}
	delete(item, "textEdit")
}
