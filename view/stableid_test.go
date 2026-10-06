package view

import (
	"bytes"
	"context"
	"crypto/rand"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/a-h/templ"
	"github.com/paulmanoni/nexus/v2"
	"github.com/paulmanoni/nexus/v2/config"
)

// tipped is what a component library renders for an element given no id:
// a random one, linking the element to its tooltip.
func tipped(label string) templ.Component {
	return templ.ComponentFunc(func(ctx context.Context, w io.Writer) error {
		id := "id-" + rand.Text()
		_, err := io.WriteString(w, `<button aria-describedby="`+id+`">`+label+`</button><span role="tooltip" id="`+id+`">`+label+`</span>`)
		return err
	})
}

// tipsLive renders library markup with random ids around a spot that reads
// Count, and inside one.
type tipsLive struct {
	Count Assign[int]
}

func (p *tipsLive) Inc(ctx context.Context) error { p.Count.Set(p.Count.Get() + 1); return nil }

func (p *tipsLive) Render() templ.Component {
	return templ.ComponentFunc(func(ctx context.Context, w io.Writer) error {
		r := Record(ctx, w)
		_ = r.S(w, 1, "<nav>")
		_ = tipped("Home").Render(ctx, w)
		_ = tipped("Settings").Render(ctx, w)
		_ = r.S(w, 2, "</nav><main>")
		if r.Guard(ctx, w, "count", p) {
			_ = r.S(w, 3, "<p>")
			io.WriteString(w, strconv.Itoa(p.Count.Get()))
			_ = r.S(w, 4, "</p>")
			_ = tipped("Count").Render(ctx, w)
		}
		r.Close(w)
		_ = r.S(w, 5, "</main>")
		return nil
	})
}

var randomIDs = regexp.MustCompile(`id-[A-Z2-7]{26}\b`)

// A render names a library's random ids after their place on the page, so
// the same state renders the same: a tooltip and the element it describes
// share their id, and no two places share one.
func TestStableIDs(t *testing.T) {
	render := func() string {
		var buf bytes.Buffer
		ctx, rec := withRecorder(context.Background(), &buf)
		_ = (&tipsLive{}).Render().Render(ctx, rec.w)
		root := rec.tree()
		if html := root.html(); html != buf.String() {
			t.Fatalf("the tree and the markup differ:\n%s\n%s", html, buf.String())
		}
		return buf.String()
	}
	a, b := render(), render()
	if a != b {
		t.Fatalf("two renders of one state differ:\n%s\n%s", a, b)
	}
	if randomIDs.MatchString(a) {
		t.Fatalf("a random id is left:\n%s", a)
	}
	ids := regexp.MustCompile(`id="([^"]+)"`).FindAllStringSubmatch(a, -1)
	seen := map[string]bool{}
	for _, m := range ids {
		if seen[m[1]] {
			t.Errorf("two places share id %s", m[1])
		}
		seen[m[1]] = true
		if !strings.Contains(a, `aria-describedby="`+m[1]+`"`) {
			t.Errorf("id %s lost its element", m[1])
		}
	}
	if len(seen) != 3 {
		t.Fatalf("%d ids, want 3:\n%s", len(seen), a)
	}

	for _, s := range []string{"id-" + rand.Text() + "x", "xid-" + rand.Text(), "id-" + strings.ToLower(rand.Text()), "id-ABC"} {
		r := &recorder{stack: []*recNode{{}}}
		if got := r.stableIDs(s); got != s {
			t.Errorf("%q is not a random id, but became %q", s, got)
		}
	}
}

// A live page with such ids joins its HTTP render, and its events are
// skipping renders that match full ones.
func TestStableIDsLive(t *testing.T) {
	var reported []string
	var mu sync.Mutex
	was := reportStale
	reportStale = func(_ reflect.Type, labels []string) {
		mu.Lock()
		reported = append(reported, labels...)
		mu.Unlock()
	}
	t.Cleanup(func() { reportStale = was })
	wasVerify := verifyTracking
	verifyTracking = true
	t.Cleanup(func() { verifyTracking = wasVerify })

	dropParked()
	t.Cleanup(dropParked)
	app, stop, err := nexus.InProcess(config.Runtime{},
		Live[*tipsLive]("/tips").Provide(func() *tipsLive { return &tipsLive{} }),
	)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(app)
	t.Cleanup(func() { srv.Close(); _ = stop(context.Background()) })

	res, err := http.Get(srv.URL + "/tips")
	if err != nil {
		t.Fatal(err)
	}
	page, _ := io.ReadAll(res.Body)
	res.Body.Close()
	join := regexp.MustCompile(`data-nx-live-join="([^"]+)"`).FindSubmatch(page)
	if join == nil {
		t.Fatalf("no join id:\n%s", page)
	}
	c := dialLive(t, srv, "/tips/_live?join="+string(join[1]))
	if err := c.WriteJSON(liveEvent{Ref: 1, Event: "Inc"}); err != nil {
		t.Fatal(err)
	}
	first := reply(t, c)
	if first.Ref != 1 {
		t.Fatal("the connection's render differs from the HTTP one: the page was sent again")
	}
	if err := c.WriteJSON(liveEvent{Ref: 2, Event: "Inc"}); err != nil {
		t.Fatal(err)
	}
	r := reply(t, c)
	if !strings.Contains(r.HTML, "<p>2</p>") || randomIDs.MatchString(r.HTML) {
		t.Fatalf("page after two events:\n%s", r.HTML)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(reported) > 0 {
		t.Fatalf("skipping renders differed from full ones: %v", reported)
	}
}
