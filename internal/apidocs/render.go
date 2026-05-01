package apidocs

import (
	"bytes"
	"fmt"
	"html/template"
	"sort"
	"strings"
)

// resourceUse pairs a handler with its enclosing module so the
// resources panel can render "<module>.<handler>" links back into
// the main column.
type resourceUse struct {
	Module string
	Name   string
}

// resourceEntry is one row in the Resources panel: the dep type and
// every handler that injects it.
type resourceEntry struct {
	Type  string
	Slug  string
	Users []resourceUse
}

// renderCtx wraps Doc with the precomputed Resources index so the
// template can iterate it without recomputation.
type renderCtx struct {
	*Doc
	Resources []resourceEntry
}

// buildResources walks all handlers (in modules + loose) and groups
// them by injected dep type. Result is sorted by type for stable
// output. Pure types (string, int, etc.) are kept — readers can
// still see "who takes a *gin.Context".
func buildResources(d *Doc) []resourceEntry {
	byType := map[string][]resourceUse{}
	visit := func(mod string, hs []Handler) {
		for _, h := range hs {
			for _, dep := range h.Deps {
				byType[dep.Type] = append(byType[dep.Type], resourceUse{Module: mod, Name: h.Name})
			}
		}
	}
	for _, m := range d.Modules {
		visit(m.Name, m.Handlers)
	}
	visit("", d.Loose)
	out := make([]resourceEntry, 0, len(byType))
	for t, users := range byType {
		out = append(out, resourceEntry{Type: t, Slug: slugType(t), Users: users})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Type < out[j].Type })
	return out
}

// slugType produces a URL-safe anchor id for a Go type expression.
// Stable across runs so links survive page reloads.
func slugType(t string) string {
	var b strings.Builder
	b.WriteString("type-")
	for _, r := range t {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == '*':
			b.WriteString("ptr-")
		case r == '.':
			b.WriteRune('-')
		case r == '[':
			b.WriteString("-of-")
		case r == ']':
			// drop
		default:
			b.WriteRune('-')
		}
	}
	return b.String()
}

// RenderHTML produces a self-contained HTML document for the given IR.
// The template + CSS are embedded — no external assets, no JS — so the
// output works behind any HTTP server, including being saved to disk
// and opened straight from the filesystem.
//
// The viewer is intentionally minimal: a left-rail TOC of modules and
// handlers, a main column of cards (one per handler) with doc, deps,
// args table, return, and options. Renderers for richer formats
// (PDF via chromedp, Vue dashboard tab) consume the same IR.
func RenderHTML(doc *Doc) ([]byte, error) {
	tpl, err := template.New("apidocs").Funcs(funcMap).Parse(htmlTemplate)
	if err != nil {
		return nil, fmt.Errorf("parse template: %w", err)
	}
	var buf bytes.Buffer
	ctx := &renderCtx{Doc: doc, Resources: buildResources(doc)}
	if err := tpl.Execute(&buf, ctx); err != nil {
		return nil, fmt.Errorf("execute template: %w", err)
	}
	return buf.Bytes(), nil
}

// funcMap provides the small set of helpers the template needs:
// kind→css-class, anchor IDs, and pretty kind labels. Kept here so
// the template stays readable.
var funcMap = template.FuncMap{
	"kindClass": func(k string) string { return "k-" + k },
	"kindLabel": func(k string) string {
		switch k {
		case "query":
			return "Query"
		case "mutation":
			return "Mutation"
		case "rest":
			return "REST"
		case "ws":
			return "WS"
		default:
			return k
		}
	},
	"anchor": func(mod, name string) string {
		return strings.ToLower(mod + "." + name)
	},
	"hasArgs": func(a *Args) bool {
		return a != nil && (a.Type != "" || len(a.Fields) > 0)
	},
	"slugType": slugType,
	"editorURL": func(file string, line int) template.URL {
		// vscode://file/<absolute path>:<line> opens the file in VS Code
		// when the protocol handler is registered (default install). For
		// users on other editors the raw file:line text remains visible
		// in the link's anchor text — copy/paste still works.
		//
		// template.URL bypasses html/template's allowlist (vscode:// is
		// not on it). Safe here because we build the string ourselves
		// from collector-provided absolute paths and integer lines.
		return template.URL(fmt.Sprintf("vscode://file/%s:%d", file, line))
	},
}

const htmlTemplate = `<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<title>{{.Module}} — API reference</title>
<style>
  :root {
    --bg: #0e1116;
    --panel: #161b22;
    --border: #30363d;
    --fg: #e6edf3;
    --muted: #8b949e;
    --accent: #58a6ff;
    --tag: #1f6feb33;
    --code: #1f242c;
    --k-query: #3fb950;
    --k-mutation: #d29922;
    --k-rest: #58a6ff;
    --k-ws: #bc8cff;
  }
  * { box-sizing: border-box; }
  body {
    margin: 0;
    background: var(--bg);
    color: var(--fg);
    font: 14px/1.5 -apple-system, BlinkMacSystemFont, "Segoe UI", system-ui, sans-serif;
    display: grid;
    grid-template-columns: 280px 1fr;
    min-height: 100vh;
  }
  aside {
    background: var(--panel);
    border-right: 1px solid var(--border);
    padding: 20px 16px;
    position: sticky;
    top: 0;
    height: 100vh;
    overflow-y: auto;
  }
  aside h1 { font-size: 14px; margin: 0 0 16px; color: var(--muted); text-transform: uppercase; letter-spacing: .08em; }
  aside .mod { margin-bottom: 16px; }
  aside .mod-name { font-weight: 600; color: var(--fg); margin-bottom: 4px; }
  aside ul { list-style: none; padding: 0; margin: 0; }
  aside li { margin: 2px 0; }
  aside a { color: var(--muted); text-decoration: none; font-size: 13px; }
  aside a:hover { color: var(--accent); }
  main { padding: 32px 40px; max-width: 980px; }
  h2.module { font-size: 22px; margin: 32px 0 4px; padding-top: 8px; }
  h2.module:first-child { margin-top: 0; }
  .module-meta { color: var(--muted); font-size: 13px; margin-bottom: 20px; }
  .module-doc { color: var(--fg); margin: 0 0 24px; white-space: pre-wrap; }
  .card {
    background: var(--panel);
    border: 1px solid var(--border);
    border-radius: 8px;
    padding: 20px 24px;
    margin: 0 0 16px;
  }
  .card-head { display: flex; align-items: baseline; gap: 12px; margin-bottom: 8px; }
  .kind {
    font-size: 11px;
    font-weight: 700;
    padding: 2px 8px;
    border-radius: 4px;
    text-transform: uppercase;
    letter-spacing: .05em;
  }
  .k-query    { background: var(--k-query); color: #051a09; }
  .k-mutation { background: var(--k-mutation); color: #1b1300; }
  .k-rest     { background: var(--k-rest); color: #001226; }
  .k-ws       { background: var(--k-ws); color: #1b0f2e; }
  .op-name { font-size: 18px; font-weight: 600; }
  .op-path { color: var(--muted); font-family: ui-monospace, SFMono-Regular, monospace; font-size: 13px; }
  .doc { white-space: pre-wrap; color: var(--fg); margin: 8px 0 16px; }
  .section-label { color: var(--muted); font-size: 11px; text-transform: uppercase; letter-spacing: .08em; margin: 16px 0 8px; }
  table { width: 100%; border-collapse: collapse; }
  th, td { text-align: left; padding: 6px 8px; border-bottom: 1px solid var(--border); font-size: 13px; vertical-align: top; }
  th { color: var(--muted); font-weight: 500; }
  code, .mono { font-family: ui-monospace, SFMono-Regular, monospace; font-size: 12.5px; }
  td.mono, td code { background: var(--code); padding: 1px 6px; border-radius: 4px; }
  .tag { display: inline-block; background: var(--tag); color: var(--accent); padding: 1px 6px; border-radius: 4px; margin-right: 4px; font-size: 11px; font-family: ui-monospace, monospace; }
  .badge-opt { color: var(--muted); font-size: 11px; margin-left: 6px; }
  .deps li, .opts li { padding: 4px 0; border-bottom: 1px dotted var(--border); }
  .deps li:last-child, .opts li:last-child { border: 0; }
  .opts pre { margin: 0; white-space: pre-wrap; font-family: ui-monospace, monospace; font-size: 12.5px; color: var(--fg); }
  .empty { color: var(--muted); font-style: italic; font-size: 13px; }
  .pos { color: var(--muted); font-size: 11px; font-family: ui-monospace, monospace; margin-top: 12px; }
  .pos a { color: var(--muted); text-decoration: none; border-bottom: 1px dotted var(--border); }
  .pos a:hover { color: var(--accent); border-color: var(--accent); }
  .deps a { color: var(--accent); text-decoration: none; }
  .deps a:hover { text-decoration: underline; }
  .resources { margin-top: 48px; }
  .resources h2 { font-size: 18px; color: var(--muted); text-transform: uppercase; letter-spacing: .08em; margin: 0 0 16px; }
  .res-row { display: grid; grid-template-columns: 280px 1fr; gap: 16px; padding: 10px 0; border-bottom: 1px solid var(--border); }
  .res-row code { background: var(--code); padding: 2px 6px; border-radius: 4px; }
  .res-users a { display: inline-block; margin-right: 12px; color: var(--accent); text-decoration: none; font-size: 13px; }
  .res-users a:hover { text-decoration: underline; }
  aside .toc-resources { border-top: 1px solid var(--border); padding-top: 12px; margin-top: 16px; }
  .warn-banner {
    background: #d29922;
    color: #1b1300;
    padding: 10px 16px;
    border-radius: 6px;
    margin-bottom: 20px;
    font-size: 13px;
  }
  .warn-banner strong { font-weight: 700; }
  .entities { margin-top: 48px; }
  .entities h2 { font-size: 18px; color: var(--muted); text-transform: uppercase; letter-spacing: .08em; margin: 0 0 16px; }
  .entity-card {
    background: var(--panel);
    border: 1px solid var(--border);
    border-radius: 8px;
    padding: 16px 20px;
    margin: 0 0 14px;
  }
  .entity-head { display: flex; align-items: baseline; justify-content: space-between; gap: 12px; margin-bottom: 8px; }
  .entity-name { font-size: 16px; font-weight: 600; font-family: ui-monospace, monospace; }
  .entity-pkg { color: var(--muted); font-size: 12px; font-family: ui-monospace, monospace; }
  .typed { color: var(--accent); text-decoration: none; }
  .typed:hover { text-decoration: underline; }
  .methods { margin-top: 12px; }
  .method-row {
    padding: 8px 0;
    border-top: 1px dotted var(--border);
  }
  .method-row:first-child { border-top: 0; }
  .method-sig { font-family: ui-monospace, monospace; font-size: 12.5px; }
  .method-sig .mname { color: var(--accent); font-weight: 600; }
  .method-recv { color: var(--muted); font-size: 11px; margin-left: 6px; }
  .method-doc { color: var(--fg); font-size: 12.5px; margin: 4px 0 0; white-space: pre-wrap; }
  .pdf-btn {
    display: block;
    width: 100%;
    background: var(--accent);
    color: #001226;
    border: 0;
    border-radius: 6px;
    padding: 8px 12px;
    font-size: 13px;
    font-weight: 600;
    cursor: pointer;
    margin-bottom: 16px;
    font-family: inherit;
  }
  .pdf-btn:hover { background: #79b8ff; }
  .pdf-hint { color: var(--muted); font-size: 11px; margin: -8px 0 16px; line-height: 1.4; }

  /* Print stylesheet — invoked by Cmd-P / window.print().
     Browser's "Save as PDF" target produces a clean document:
     no sidebar, light background, page-break per module so each
     service starts on a fresh page. */
  @media print {
    :root {
      --bg: #ffffff;
      --panel: #ffffff;
      --border: #d0d7de;
      --fg: #1f2328;
      --muted: #59636e;
      --accent: #0969da;
      --tag: #ddf4ff;
      --code: #f6f8fa;
    }
    body { display: block; font-size: 11pt; }
    aside { display: none; }
    main { padding: 0; max-width: none; }
    .card {
      break-inside: avoid;
      page-break-inside: avoid;
      box-shadow: none;
      margin-bottom: 12px;
    }
    h2.module {
      break-before: page;
      page-break-before: always;
    }
    h2.module:first-of-type {
      break-before: auto;
      page-break-before: auto;
    }
    .pos a { color: var(--muted); border: 0; }
    .deps a, .res-users a { color: var(--accent); text-decoration: none; }
    .resources, .entities { break-before: page; page-break-before: always; }
    .entity-card { break-inside: avoid; page-break-inside: avoid; box-shadow: none; }
    /* Headers/footers are added by the print dialog; we just provide
       a title at the top of the first page via the document <title>. */
    @page { margin: 18mm 16mm; }
  }
</style>
</head>
<body>
<aside>
  <h1>{{.Module}}</h1>
  <button class="pdf-btn" onclick="window.print()">Save as PDF</button>
  <div class="pdf-hint">Opens the browser's print dialog. Pick "Save as PDF" as the destination.</div>
  {{range .Modules}}
  <div class="mod">
    <div class="mod-name"><a href="#mod-{{.Name}}">{{.Name}}</a></div>
    <ul>
      {{range .Handlers}}
      <li>
        <a href="#{{anchor $.Module .Name}}">{{.Name}}</a>
        <span class="badge-opt">{{kindLabel .Kind}}</span>
      </li>
      {{end}}
    </ul>
  </div>
  {{end}}
  {{if .Resources}}
  <div class="toc-resources">
    <div class="mod-name"><a href="#resources">Resources</a></div>
    <ul>
      {{range .Resources}}<li><a href="#{{.Slug}}"><code>{{.Type}}</code></a></li>{{end}}
    </ul>
  </div>
  {{end}}
  {{if .Entities}}
  <div class="toc-resources">
    <div class="mod-name"><a href="#entities">Entities</a></div>
    <ul>
      {{range .Entities}}<li><a href="#{{.Slug}}"><code>{{.Name}}</code></a></li>{{end}}
    </ul>
  </div>
  {{end}}
</aside>

<main>
{{if .LoadErrors}}
<div class="warn-banner">
  <strong>Partial scan:</strong> {{.LoadErrors}} package load error(s) — IR shows whatever did parse.
  Stale third-party imports or broken local packages are usually the cause.
</div>
{{end}}
{{range .Modules}}
  <h2 class="module" id="mod-{{.Name}}">{{.Name}}</h2>
  <div class="module-meta">
    {{- if .RoutePath}}prefix <code>{{.RoutePath}}</code> · {{end -}}
    {{len .Handlers}} endpoint{{if ne (len .Handlers) 1}}s{{end}}
    {{- if .Provides}} · provides {{range $i, $p := .Provides}}{{if $i}}, {{end}}<code>{{$p}}</code>{{end}}{{end}}
  </div>
  {{if .Doc}}<div class="module-doc">{{.Doc}}</div>{{end}}

  {{range .Handlers}}
  <div class="card" id="{{anchor $.Module .Name}}">
    <div class="card-head">
      <span class="kind {{kindClass .Kind}}">{{kindLabel .Kind}}</span>
      <span class="op-name">{{.Name}}</span>
      {{if .HTTP}}<span class="op-path">{{.HTTP.Method}} {{.HTTP.Path}}</span>{{end}}
      {{if .WS}}<span class="op-path">{{.WS.Path}} · type "{{.WS.MessageType}}"</span>{{end}}
    </div>

    {{if .Doc}}<div class="doc">{{.Doc}}</div>{{end}}

    {{if .Deps}}
    <div class="section-label">Dependencies</div>
    <ul class="deps">
      {{range .Deps}}<li>
        <span class="mono">{{.Name}}</span>
        <a href="#{{slugType .Type}}"><code>{{.Type}}</code></a>
        {{if .TypeLink}}<a class="typed" href="#{{.TypeLink}}">→ definition</a>{{end}}
      </li>{{end}}
    </ul>
    {{end}}

    {{if hasArgs .Args}}
    <div class="section-label">Arguments
      {{if .Args.Type}}{{if .Args.TypeLink}}<a class="typed" href="#{{.Args.TypeLink}}"><code>{{.Args.Type}}</code></a>{{else}}<code>{{.Args.Type}}</code>{{end}}{{end}}
    </div>
    {{if .Args.Fields}}
    <table>
      <thead><tr><th>Field</th><th>Type</th><th>Tags</th></tr></thead>
      <tbody>
        {{range .Args.Fields}}
        <tr>
          <td class="mono">{{.Name}}{{if .Optional}} <span class="badge-opt">optional</span>{{end}}</td>
          <td>{{if .TypeLink}}<a class="typed" href="#{{.TypeLink}}"><code>{{.Type}}</code></a>{{else}}<code>{{.Type}}</code>{{end}}</td>
          <td>
            {{range $k, $v := .Tags}}<span class="tag">{{$k}}: {{$v}}</span>{{end}}
          </td>
        </tr>
        {{end}}
      </tbody>
    </table>
    {{else}}
    <div class="empty">No fields.</div>
    {{end}}
    {{end}}

    {{if .Returns}}
    <div class="section-label">Returns</div>
    <div>
      {{if .ReturnsLink}}<a class="typed" href="#{{.ReturnsLink}}"><code>{{.Returns}}</code></a>{{else}}<code>{{.Returns}}</code>{{end}}
    </div>
    {{end}}

    {{if .Options}}
    <div class="section-label">Options</div>
    <ul class="opts">
      {{range .Options}}<li><pre>{{.Expr}}</pre></li>{{end}}
    </ul>
    {{end}}

    <div class="pos"><a href="{{editorURL .Pos.File .Pos.Line}}">{{.Pos.File}}:{{.Pos.Line}}</a></div>
  </div>
  {{end}}
{{end}}

{{if .Resources}}
  <section class="resources">
    <h2 id="resources">Resources</h2>
    {{range .Resources}}
    <div class="res-row" id="{{.Slug}}">
      <div><code>{{.Type}}</code></div>
      <div class="res-users">
        {{range .Users}}<a href="#{{anchor .Module .Name}}">{{if .Module}}{{.Module}}.{{end}}{{.Name}}</a>{{end}}
      </div>
    </div>
    {{end}}
  </section>
{{end}}

{{if .Entities}}
  <section class="entities">
    <h2 id="entities">Entities</h2>
    {{range .Entities}}
    <div class="entity-card" id="{{.Slug}}">
      <div class="entity-head">
        <span class="entity-name">{{.Name}}</span>
        <span class="entity-pkg">{{.Pkg}}</span>
      </div>
      {{if .Doc}}<div class="doc">{{.Doc}}</div>{{end}}
      {{if .Fields}}
      <table>
        <thead><tr><th>Field</th><th>Type</th><th>Tags</th></tr></thead>
        <tbody>
          {{range .Fields}}
          <tr>
            <td class="mono">{{.Name}}</td>
            <td>{{if .TypeLink}}<a class="typed" href="#{{.TypeLink}}"><code>{{.Type}}</code></a>{{else}}<code>{{.Type}}</code>{{end}}</td>
            <td>{{range $k, $v := .Tags}}<span class="tag">{{$k}}: {{$v}}</span>{{end}}</td>
          </tr>
          {{end}}
        </tbody>
      </table>
      {{else if not .Methods}}
      <div class="empty">Opaque type (non-struct or unexported fields only).</div>
      {{end}}
      {{if .Methods}}
      <div class="section-label">Methods</div>
      <div class="methods">
        {{range .Methods}}
        <div class="method-row">
          <div class="method-sig"><span class="mname">{{.Name}}</span>{{.Signature}}{{if .Receiver}}<span class="method-recv">{{.Receiver}} receiver</span>{{end}}</div>
          {{if .Doc}}<div class="method-doc">{{.Doc}}</div>{{end}}
        </div>
        {{end}}
      </div>
      {{end}}
      <div class="pos"><a href="{{editorURL .Pos.File .Pos.Line}}">{{.Pos.File}}:{{.Pos.Line}}</a></div>
    </div>
    {{end}}
  </section>
{{end}}

{{if .Loose}}
  <h2 class="module">Unscoped</h2>
  <div class="module-meta">{{len .Loose}} handler{{if ne (len .Loose) 1}}s{{end}} registered outside any nexus.Module</div>
  {{range .Loose}}
  <div class="card">
    <div class="card-head">
      <span class="kind {{kindClass .Kind}}">{{kindLabel .Kind}}</span>
      <span class="op-name">{{.Name}}</span>
    </div>
    {{if .Doc}}<div class="doc">{{.Doc}}</div>{{end}}
  </div>
  {{end}}
{{end}}
</main>
</body>
</html>
`