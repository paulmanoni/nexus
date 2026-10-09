package view_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/a-h/templ"

	"github.com/paulmanoni/nexus/v2"
	"github.com/paulmanoni/nexus/v2/config"
	"github.com/paulmanoni/nexus/v2/dev"
	"github.com/paulmanoni/nexus/v2/view"
	"github.com/paulmanoni/nexus/v2/view/viewtest"
)

// shelf is a whole document, so nexus dev gives it the debug toolbar.
type shelf struct{ N view.Assign[int] }

func (s *shelf) Mount(ctx context.Context) error { s.N.Set(1); return nil }
func (s *shelf) Add(ctx context.Context) error   { s.N.Set(s.N.Get() + 1); return nil }

func (s *shelf) Render() templ.Component {
	return templ.ComponentFunc(func(ctx context.Context, w io.Writer) error {
		io.WriteString(w, `<!doctype html><html><head>`)
		if err := view.Script().Render(ctx, w); err != nil {
			return err
		}
		io.WriteString(w, `</head><body><p id="n">`+strconv.Itoa(s.N.Get())+`</p><button id="add" onclick="`+view.Send(s.Add).Call+`"></button></body></html>`)
		return nil
	})
}

// Under nexus dev a live page's socket work — its connected Mount, each
// event — is listed in the debug toolbar under the page that opened it.
func TestToolbarListsLiveWork(t *testing.T) {
	t.Setenv(dev.Env, "1")
	app, stop, err := nexus.InProcess(config.Runtime{TraceCapacity: 1000},
		view.Live[*shelf]("/shelf").Provide(func() *shelf { return &shelf{} }))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = stop(context.Background()) })

	p := viewtest.Mount[*shelf](t, app)
	page := p.Eval(`document.querySelector("script[data-nexus-toolbar]").getAttribute("data-nexus-toolbar")`)
	if page == "" {
		t.Fatal("the page has no toolbar")
	}
	p.Click("#add")
	p.Expect("#n").Text("2")

	var labels []string
	for deadline := time.Now().Add(3 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		w := httptest.NewRecorder()
		app.ServeHTTP(w, httptest.NewRequest("GET", "/__nexus/toolbar/requests/"+page+"/children", nil))
		var got struct {
			Items []struct {
				Label string
				Page  bool
				Path  string
			} `json:"items"`
		}
		_ = json.Unmarshal(w.Body.Bytes(), &got)
		labels = labels[:0]
		for _, it := range got.Items {
			labels = append(labels, fmt.Sprintf("%s page=%v path=%s", it.Label, it.Page, it.Path))
		}
		if len(labels) >= 2 {
			break
		}
	}
	joined := strings.Join(labels, "\n")
	// The connection's Mount is a page entry, by its URL path, which the
	// toolbar pins when the browser navigates to it; an event is not.
	if !strings.Contains(joined, "LIVE /shelf · 200") || !strings.Contains(joined, "page=true path=/shelf") ||
		!strings.Contains(joined, "LIVE view_test.shelf.Add · 200") || !strings.Contains(joined, "page=false path=view_test.shelf.Add") {
		t.Fatalf("toolbar entries:\n%s", joined)
	}
}
