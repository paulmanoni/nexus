package view

import (
	"context"
	"io"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/a-h/templ"

	"github.com/paulmanoni/nexus/v2"
	"github.com/paulmanoni/nexus/v2/config"
)

type badgeCount struct{ N int }

var badgeSeen int

// badgeLive is a tracked page: its "badge" spot reads only a context
// processor's value, which no Assign follows.
type badgeLive struct {
	LiveView
	Count Assign[int]
}

func (p *badgeLive) Inc(ctx context.Context) error { p.Count.Set(p.Count.Get() + 1); return nil }

func (p *badgeLive) Render() templ.Component {
	return templ.ComponentFunc(func(ctx context.Context, w io.Writer) error {
		r := Record(ctx, w)
		ctx = Enter(ctx, "Badge")
		_ = r.S(w, 1, "<b>")
		if r.Guard(ctx, w, "count", p) {
			io.WriteString(w, strconv.Itoa(p.Count.Get()))
		}
		r.Close(w)
		_ = r.S(w, 2, "</b><i>")
		if r.Guard(ctx, w, "badge", p) {
			io.WriteString(w, "badge-"+strconv.Itoa(FromContext[badgeCount](ctx).N))
		}
		r.Close(w)
		_ = r.S(w, 3, "</i>")
		return nil
	})
}

// A spot that reads a context processor renders on every event: its value
// may change with no Assign changing.
func TestContextProcessorSpotsAlwaysRender(t *testing.T) {
	noVerify(t)
	dropParked()
	t.Cleanup(dropParked)
	badgeSeen = 0
	app, stop, err := nexus.InProcess(config.Runtime{},
		ContextProcessor(func(ctx context.Context) badgeCount { badgeSeen++; return badgeCount{N: badgeSeen} }),
		Live[*badgeLive]("/badge").Provide(func() *badgeLive { return &badgeLive{} }),
	)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(app)
	t.Cleanup(func() { srv.Close(); _ = stop(context.Background()) })

	c := dialLive(t, srv, "/badge/_live")
	first := reply(t, c)
	if !strings.Contains(first.HTML, "<i>badge-") {
		t.Fatalf("first render = %s", first.HTML)
	}
	before := badgeSeen
	if err := c.WriteJSON(liveEvent{Event: "Inc", Ref: 1}); err != nil {
		t.Fatal(err)
	}
	r := reply(t, c)
	if r.Error != "" {
		t.Fatal(r.Error)
	}
	if badgeSeen != before+1 || !strings.Contains(r.HTML, "badge-"+strconv.Itoa(badgeSeen)) {
		t.Errorf("after an event the badge spot shows %q (processor ran %d times since)", r.HTML, badgeSeen-before)
	}
}
