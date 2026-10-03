package main

import (
	"context"
	"strings"
	"testing"

	"github.com/paulmanoni/nexus/v2"
	"github.com/paulmanoni/nexus/v2/config"
	"github.com/paulmanoni/nexus/v2/view/viewtest"
	"github.com/paulmanoni/nexus/view/example/v2/pets"
)

// The home page in the test browser: signals, bindings and if-branches run
// in the browser runtime, shards re-render on the server.
func TestHomeInBrowser(t *testing.T) {
	p := viewtest.Get(t, boot(t), "/")

	p.Expect("#count").Text("0")
	p.Expect("#reset").Disabled()
	p.Expect("#five").Hidden()
	for range 5 {
		p.Click("#inc")
	}
	p.Expect("#count").Text("5")
	p.Expect("#five").Visible()
	p.Expect("#more").Hidden()
	p.Expect("#reset").Enabled()
	p.Click("#reset").Expect("#count").Text("0")

	p.Expect("#details").Hidden()
	p.Click("#toggle").Expect("#details").Visible()

	// The search box, the summary and the results share one signal; the
	// results are a shard the server re-renders.
	p.Expect("#summary").Hidden()
	p.Fill("#q", "cat")
	p.Expect("#summary").Visible().Text("— searching cat")
	p.Expect("#pets").ContainsText("Mochi")
	if txt := p.Text("#pets"); strings.Contains(txt, "Biscuit") {
		t.Fatalf("results for cat: %s", txt)
	}

	// Paged owns its page signal; the shard re-renders the list.
	p.Expect("#page").Text("page 1 of 3")
	p.Expect("#prev").Disabled()
	p.Click("#next")
	p.Expect("#page").Text("page 2 of 3")
	p.Expect("#paged").ContainsText("Kiwi")
	p.Expect("#prev").Enabled()

	// In-app navigation to the live board.
	p.Click("#nav-board")
	p.Expect("h1").Text("Adoption board")
	p.Expect("#adopt-Biscuit").Exists()
}

// With CSRF on — any cookie session, cookie auth or Inertia turns it on —
// shard re-renders carry the token the page's first GET set.
func TestShardsWithCSRF(t *testing.T) {
	h, stop, err := nexus.InProcess(config.Runtime{Middleware: config.Middleware{Security: &config.Security{CSRF: new(true)}}}, app()...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = stop(context.Background()) })

	p := viewtest.Get(t, h, "/")
	p.Fill("#q", "cat")
	p.Expect("#pets").ContainsText("Mochi")
	if txt := p.Text("#pets"); strings.Contains(txt, "Biscuit") {
		t.Fatalf("results for cat: %s", txt)
	}
}

// The live board end to end: events, the add form and its validation, a
// browser-only signal surviving patches, and a broadcast to another page.
func TestBoardInBrowser(t *testing.T) {
	app := boot(t)
	p := viewtest.Mount[*pets.Board](t, app)
	other := viewtest.Mount[*pets.Board](t, app)

	p.Fill("#note", "keep me")
	p.Click("#adopt-Biscuit")
	p.Expect("#adopted").Text("1")
	p.Expect("#return-Biscuit").Exists()
	p.Expect("#adopt-Biscuit").Absent()
	p.Expect("#note").Value("keep me") // the signal wins over the server's copy
	other.Expect("#adopted").Text("1") // broadcast

	// Validation as the form is typed into, and on submit.
	p.Click("#add-submit")
	p.Expect("#name-err").Text("a name is required")
	p.Fill("name", "Ziggy")
	p.Expect("#name-err").Text("")
	p.Expect("#kind-err").Text("what kind of pet?")
	p.Fill("kind", "hamster").Click("#add-submit")
	p.Expect("#pet-Ziggy").Exists()
	p.Expect("name").Value("") // the form reset after the add
	p.Expect("kind").Value("")
	p.Expect("#kind-err").Text("")
	other.Expect("#pet-Ziggy").Exists()

	p.Fill("name", "Ziggy").Click("#add-submit")
	p.Expect("#name-err").Text("Ziggy is already here")
	p.Expect("name").Value("Ziggy") // an invalid submit keeps what was typed

	p.Click("#clear")
	p.Expect("#adopted").Text("0")
	other.Expect("#adopt-Biscuit").Exists()
}
