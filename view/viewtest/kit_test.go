package viewtest_test

import (
	"context"
	"io"
	"strconv"
	"testing"

	"github.com/a-h/templ"

	"github.com/paulmanoni/nexus/v2/config"
	"github.com/paulmanoni/nexus/v2/nexustest"
	"github.com/paulmanoni/nexus/v2/view"
	"github.com/paulmanoni/nexus/v2/view/ui"
	"github.com/paulmanoni/nexus/v2/view/viewtest"
)

// Kit uses the component kit's browser-side dialog and tabs, which run as
// JS commands, and re-renders on Bump.
type Kit struct {
	view.LiveView
	N int
}

func (k *Kit) Bump(ctx context.Context) error { k.N++; return nil }

func (k *Kit) Render() templ.Component {
	return templ.ComponentFunc(func(ctx context.Context, w io.Writer) error {
		w.Write([]byte(`<!DOCTYPE html><html><head><title>Kit</title>`))
		if err := view.Script().Render(ctx, w); err != nil {
			return err
		}
		if err := ui.Script().Render(ctx, w); err != nil {
			return err
		}
		io.WriteString(w, `</head><body><p id="n">`+strconv.Itoa(k.N)+`</p><button id="bump" onclick="`+view.Send(k.Bump).Call+`">bump</button>`)
		io.WriteString(w, `<button id="open-help" data-ui-open="help">help</button>`)
		dialog := ui.Dialog(ui.DialogProps{ID: "help", Title: "Help"})
		body := templ.Raw(`<input id="help-field" name="q"><button id="help-done" type="button" data-ui-close>done</button>`)
		if err := dialog.Render(templ.WithChildren(ctx, body), w); err != nil {
			return err
		}
		tabs := ui.Tabs(ui.TabsProps{ID: "strip", Tabs: []ui.Tab{
			{ID: "tab-a", Label: "A", Panel: "panel-a", Active: true},
			{ID: "tab-b", Label: "B", Panel: "panel-b"},
		}})
		if err := tabs.Render(ctx, w); err != nil {
			return err
		}
		for _, p := range []struct {
			id     string
			active bool
		}{{"panel-a", true}, {"panel-b", false}} {
			if err := ui.TabPanel(p.id, p.active).Render(templ.WithChildren(ctx, templ.Raw(p.id)), w); err != nil {
				return err
			}
		}
		_, err := io.WriteString(w, `</body></html>`)
		return err
	})
}

func TestKitDialogAndTabs(t *testing.T) {
	app := nexustest.New(t, config.Runtime{},
		view.Live[*Kit]("/kit").Provide(func() *Kit { return &Kit{} }),
	)
	p := viewtest.Mount[*Kit](t, app)

	// The dialog opens on its field — not its close button — and closes back
	// to its opener, by a close element, its X and Esc alike.
	p.Expect("#help").Hidden()
	p.Click("#open-help")
	p.Expect("#help").Visible()
	p.Expect("#help-field").Focused()
	p.Click("#help-done")
	p.Expect("#help").Hidden()
	p.Expect("#open-help").Focused()

	p.Click("#open-help")
	p.Click("#help [data-ui-dialog-close]")
	p.Expect("#help").Hidden()

	p.Click("#open-help")
	p.Press("#help-field", "Escape")
	p.Expect("#help").Hidden()

	// Tabs switch their panels, and a re-render keeps the choice.
	p.Expect("#panel-a").Visible()
	p.Expect("#panel-b").Hidden()
	p.Click("#tab-b")
	p.Expect("#tab-b").Attr("aria-selected", "true")
	p.Expect("#tab-a").Attr("aria-selected", "false")
	p.Expect("#panel-b").Visible()
	p.Expect("#panel-a").Hidden()
	p.Click("#bump").Wait()
	p.Expect("#n").Text("1")
	p.Expect("#tab-b").Attr("aria-selected", "true")
	p.Expect("#panel-b").Visible()
	p.Expect("#panel-a").Hidden()

	if c := p.Console(); len(c) > 0 {
		t.Errorf("console: %v", c)
	}
}
