package ui

import (
	"github.com/a-h/templ"

	"github.com/paulmanoni/nexus/v2/view"
)

// tabSelect is what choosing t does in the browser, as JS commands: t alone
// is selected in its strip, its panel alone is shown (a Panel tab), then its
// OnSelect runs. A live re-render keeps the choice until the server renders
// the strip differently.
func tabSelect(p TabsProps, t Tab) templ.ComponentScript {
	ops := []view.JSOp{
		view.SetAttr("aria-selected", "false", "[data-ui-tab]", view.Within("[data-ui-tabs]")),
		view.SetAttr("aria-selected", "true", ""),
	}
	if t.Panel != "" {
		for _, o := range p.Tabs {
			if o.Panel != "" && o.Panel != t.Panel {
				ops = append(ops, view.Hide("#"+o.Panel))
			}
		}
		ops = append(ops, view.Show("#"+t.Panel))
	}
	if hasScript(t.OnSelect) {
		ops = append(ops, view.Exec("data-ui-select", ""))
	}
	return view.JS(ops...)
}

// dialogCancel is what closes a browser-side dialog — its close button,
// CloseDialog, Esc and a backdrop click all run it: hide it, and give focus
// back to what opened it.
var dialogCancel = view.Commands(view.Hide(""), view.PopFocus())

// closeDialog runs the dialog's closing steps from inside it.
var closeDialog = view.JS(view.Exec("data-cancel", "[data-ui-dialog]", view.Closest()))
