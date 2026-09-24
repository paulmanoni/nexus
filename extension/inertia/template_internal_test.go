package inertia

import (
	"reflect"
	"strings"
	"testing"
	"testing/fstest"
)

// TestScanTagsSkipsNonMarkup: tag-shaped text in comments, raw-text elements
// and attribute values is not reported as a tag.
func TestScanTagsSkipsNonMarkup(t *testing.T) {
	// "<!-->" is a whole (empty) comment, so the <b> after it is markup.
	doc := `<!DOCTYPE html><!--> <b> --><!-- <i id=app> -->` +
		`<head><title>a <u> b</title>` +
		`<script>if (a </div> b) x = "<div id='app'>" + '</scrip' + "t>"</SCRIPT >` +
		`<style>p::after{content:"<em>"}</style></head>` +
		`<body><p title="a > <s>" data-x='<div id="app">'>t</p>` +
		`<textarea><div id=app></textarea><noscript><div id=app></noscript>` +
		`<?php echo 1 ?></ bogus><section id="app"></section></body>`
	var got []string
	scanTags([]byte(doc), func(tag htmlTag) bool {
		name := tag.name
		if tag.end {
			name = "/" + name
		}
		got = append(got, name)
		return true
	})
	want := []string{"b", "head", "title", "/title", "script", "/script", "style", "/style", "/head",
		"body", "p", "/p", "textarea", "/textarea", "noscript", "/noscript", "section", "/section", "/body"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("tags:\n got %v\nwant %v", got, want)
	}

	tp, ok := parseTemplate([]byte(doc), "app")
	if !ok || tp.mount.name != "section" {
		t.Fatalf("mount: ok=%v name=%q; want the <section>", ok, tp.mount.name)
	}
}

// TestParseTemplateAttributeForms: the id is matched however the attribute is
// written, an existing data-page is replaced in any quoting, and the new
// attributes land inside the tag.
func TestParseTemplateAttributeForms(t *testing.T) {
	cases := map[string]string{
		`<div id=app>`:                          `<div id=app data-page="{}">`,
		`<div id='app'>`:                        `<div id='app' data-page="{}">`,
		`<DIV ID="app">`:                        `<DIV ID="app" data-page="{}">`,
		`<div class=a id = "app" >`:             `<div class=a id = "app"  data-page="{}">`,
		`<div id="&#97;pp">`:                    `<div id="&#97;pp" data-page="{}">`,
		`<div id="app"/>`:                       `<div id="app" data-page="{}"/>`,
		`<div data-page={"a":1} id=app>`:        `<div id=app data-page="{}">`,
		`<div id="app" data-page='{"x":"y"}'/>`: `<div id="app" data-page="{}"/>`,
		`<div id="app"data-page="x">`:           `<div id="app" data-page="{}">`,
	}
	for in, want := range cases {
		tp, ok := parseTemplate([]byte(in+"</div>"), "app")
		if !ok {
			t.Errorf("%s: mount not found", in)
			continue
		}
		got := string(tp.render([]byte("{}"), "", SSRResult{}, ""))
		if got != want+"</div>" {
			t.Errorf("%s:\n got %s\nwant %s</div>", in, got, want)
		}
	}
	for _, in := range []string{`<div id="apps">`, `<div xid="app">`, `<div data-id="app">`, `<div id="App">`} {
		if _, ok := parseTemplate([]byte(in), "app"); ok {
			t.Errorf("%s: must not be the mount", in)
		}
	}
}

// TestParseTemplateNestedSSR: the SSR body replaces exactly the mount's
// content, past nested elements of the same name.
func TestParseTemplateNestedSSR(t *testing.T) {
	doc := `<html><head><title>T</title></head><body>` +
		`<div id="app"><div class="l"><div></div></div><!-- </div> --><p>x</p></div>` +
		`<div>after</div></body></html>`
	tp, ok := parseTemplate([]byte(doc), "app")
	if !ok {
		t.Fatal("mount not found")
	}
	got := string(tp.render([]byte(`{"a":"<b>"}`), "", SSRResult{Body: "<main/>"}, "<meta x>"))
	want := `<html><head><title>T</title><meta x></head><body>` +
		`<div id="app" data-server-rendered="true" data-page="{&#34;a&#34;:&#34;&lt;b&gt;&#34;}"><main/></div>` +
		`<div>after</div></body></html>`
	if got != want {
		t.Fatalf("\n got %s\nwant %s", got, want)
	}

	// No </head>: head content goes before <body>, or before the mount.
	for doc, want := range map[string]string{
		`<body><div id=app></div>`: `<meta x><body><div id=app data-page="{}"></div>`,
		`<div id=app></div>`:       `<meta x><div id=app data-page="{}"></div>`,
	} {
		tp, _ := parseTemplate([]byte(doc), "app")
		if got := string(tp.render([]byte("{}"), "", SSRResult{}, "<meta x>")); got != want {
			t.Errorf("\n got %s\nwant %s", got, want)
		}
	}
}

// TestStampNonce: every script, link and style in a fragment gets the nonce
// unless it has one — with or without other attributes, inline or not.
func TestStampNonce(t *testing.T) {
	in := `<script>var s = "<link x>"</script><script src=a.js></script><link href=b.css>` +
		`<style>p{}</style><script nonce="k">1</script><meta name=x>`
	want := `<script nonce="n">var s = "<link x>"</script><script nonce="n" src=a.js></script><link nonce="n" href=b.css>` +
		`<style nonce="n">p{}</style><script nonce="k">1</script><meta name=x>`
	if got := stampNonce(in, "n"); got != want {
		t.Fatalf("\n got %s\nwant %s", got, want)
	}
	if got := stampNonce(in, ""); got != in {
		t.Fatalf("no nonce must leave the fragment alone: %s", got)
	}
}

// TestDevTagsEscaping: hot-file and NEXUS_VITE_DEV URLs are escaped for where
// they land: an attribute value, and a JS string in the React preamble.
func TestDevTagsEscaping(t *testing.T) {
	const evil = `http://x/"'></script><script>alert(1)//`
	out := devTags(evil+"/@vite/client", evil+"/src/main.tsx", evil+"/@react-refresh", true, false)
	if strings.Contains(out, "<script>alert") {
		t.Fatalf("a URL broke out of its context:\n%s", out)
	}
	for _, want := range []string{
		`src="http://x/&#34;&#39;&gt;&lt;/script&gt;&lt;script&gt;alert(1)///@vite/client"`,
		`import RefreshRuntime from "http://x/\"'\u003e\u003c/script\u003e\u003cscript\u003ealert(1)///@react-refresh"`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %s in:\n%s", want, out)
		}
	}
}

// TestHeadTagsMount: manifest tags are rooted at the mount, however it is
// written, and asset names are attribute-escaped.
func TestHeadTagsMount(t *testing.T) {
	mf := `{"src/main.ts":{"file":"assets/m\"a.js","isEntry":true,"css":["assets/m.css"],"imports":["_d.js"]},"_d.js":{"file":"assets/d.js"}}`
	m, err := loadManifest(fstest.MapFS{"dist/.vite/manifest.json": {Data: []byte(mf)}}, "dist")
	if err != nil {
		t.Fatal(err)
	}
	for mount, prefix := range map[string]string{"": "", "/admin": "/admin", "/admin/": "/admin", "/api/admin": "/api/admin"} {
		got := m.headTags(mount)
		want := `<link rel="stylesheet" href="` + prefix + `/assets/m.css">` +
			`<link rel="modulepreload" href="` + prefix + `/assets/d.js">` +
			`<script type="module" src="` + prefix + `/assets/m&#34;a.js"></script>`
		if got != want {
			t.Errorf("mount %q:\n got %s\nwant %s", mount, got, want)
		}
	}
}

// TestLoadsModule: only a module script counts as Config.Head loading the
// client.
func TestLoadsModule(t *testing.T) {
	for in, want := range map[string]bool{
		`<script type="module" src="/x.js"></script>`: true,
		`<script TYPE=Module>import "/x.js"</script>`: true,
		`<script src="/analytics.js"></script>`:       false,
		`<!-- <script type="module"> -->`:             false,
		`<meta name="type" content="module">`:         false,
		``:                                            false,
	} {
		if got := loadsModule(in); got != want {
			t.Errorf("loadsModule(%s) = %v, want %v", in, got, want)
		}
	}
}
