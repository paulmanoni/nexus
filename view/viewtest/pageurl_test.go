package viewtest_test

import (
	"context"
	"fmt"
	"io"
	"testing"

	"github.com/a-h/templ"

	"github.com/paulmanoni/nexus/v2/config"
	"github.com/paulmanoni/nexus/v2/nexustest"
	"github.com/paulmanoni/nexus/v2/view"
	"github.com/paulmanoni/nexus/v2/view/viewtest"
)

type crumbPath string

func whereAmI() templ.Component {
	return templ.ComponentFunc(func(ctx context.Context, w io.Writer) error {
		_, err := fmt.Fprintf(w, `<span id="url">%s</span><span id="crumb">%s</span>`,
			view.CurrentURL(ctx).RequestURI(), view.FromContext[crumbPath](ctx))
		return err
	})
}

// Wander is a live page that patches its own URL.
type Wander struct{ view.LiveView }

func (w *Wander) Go(ctx context.Context) error { w.PushPatch("/wander?tab=2"); return nil }

func (w *Wander) Render() templ.Component {
	return templ.ComponentFunc(func(ctx context.Context, out io.Writer) error {
		io.WriteString(out, `<!DOCTYPE html><html><head><title>Wander</title>`)
		view.Script().Render(ctx, out)
		io.WriteString(out, `</head><body>`)
		whereAmI().Render(ctx, out)
		_, err := fmt.Fprintf(out, `<button id="go" onclick="%s">go</button></body></html>`, view.Send(w.Go).Call)
		return err
	})
}

func TestCurrentURL(t *testing.T) {
	app := nexustest.New(t, config.Runtime{},
		view.ContextProcessor(func(ctx context.Context) crumbPath { return crumbPath("at " + view.CurrentURL(ctx).Path) }),
		view.Page("GET", "/plain", func() templ.Component {
			return templ.ComponentFunc(func(ctx context.Context, w io.Writer) error {
				io.WriteString(w, `<!DOCTYPE html><html><head><title>Plain</title></head><body>`)
				whereAmI().Render(ctx, w)
				_, err := io.WriteString(w, `</body></html>`)
				return err
			})
		}),
		view.Live[*Wander]("/wander"),
	)

	p := viewtest.Get(t, app, "/plain?q=pets")
	p.Expect("#url").Text("/plain?q=pets")
	p.Expect("#crumb").Text("at /plain")

	w := viewtest.Mount[*Wander](t, app, viewtest.At("/wander?tab=1"))
	w.Expect("#url").Text("/wander?tab=1")
	w.Click("#go").Wait()
	w.Expect("#url").Text("/wander?tab=2")
	w.Expect("#crumb").Text("at /wander")
}
