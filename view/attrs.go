package view

import (
	"context"
	"encoding/json"
	"fmt"
	"io"

	"github.com/a-h/templ"
)

// Caps holds the values a compiled expression reads from its surroundings.
// The generator fills it in; the browser receives each value as it was at
// render time (a signal as a live reference). Everything in it is visible to
// the browser, so never let a reactive expression read a secret.
type Caps map[string]any

type spec struct {
	Fn   string `json:"fn"`
	Caps Caps   `json:"caps,omitempty"`
}

func specJSON(id string, caps Caps) string {
	b, err := json.Marshal(spec{Fn: id, Caps: caps})
	if err != nil {
		panic("view: a value a reactive expression reads is not JSON-encodable: " + err.Error())
	}
	return string(b)
}

// The functions below are what generated code calls; templates never name
// them.

// Bind renders an attribute that reads a signal: its server value plus the
// compiled expression that keeps it current.
func Bind(name, id string, caps Caps, value any) templ.OrderedAttributes {
	return templ.OrderedAttributes{
		{Key: name, Value: value},
		{Key: "data-nx-bind-" + name, Value: specJSON(id, caps)},
	}
}

// OnAttr renders a compiled action for an on* attribute.
func OnAttr(event, id string, caps Caps, _ templ.ComponentScript) templ.OrderedAttributes {
	return templ.OrderedAttributes{{Key: "data-nx-on-" + event, Value: specJSON(id, caps)}}
}

// Text renders a text expression that reads a signal.
func Text(id string, caps Caps, value any) templ.Component {
	return templ.ComponentFunc(func(ctx context.Context, w io.Writer) error {
		_, err := fmt.Fprintf(w, `<nx-t data-nx-bind-text="%s">%s</nx-t>`,
			templ.EscapeString(specJSON(id, caps)), templ.EscapeString(fmt.Sprint(value)))
		return err
	})
}

// When renders one branch of an if whose condition reads a signal. Every
// branch is rendered; the browser shows the one whose condition holds.
func When(id string, caps Caps, show bool) templ.Component {
	return templ.ComponentFunc(func(ctx context.Context, w io.Writer) error {
		hidden := ""
		if !show {
			hidden = " hidden"
		}
		if _, err := fmt.Fprintf(w, `<nx-if data-nx-bind-show="%s"%s>`, templ.EscapeString(specJSON(id, caps)), hidden); err != nil {
			return err
		}
		if children := templ.GetChildren(ctx); children != nil {
			if err := children.Render(templ.ClearChildren(ctx), w); err != nil {
				return err
			}
		}
		_, err := io.WriteString(w, `</nx-if>`)
		return err
	})
}

// Attrs merges compiled attributes into a templ.Attributes — what a
// templ.Attributes{…} literal with reactive entries compiles to, so a
// component library's Props.Attributes can carry actions and bindings.
func Attrs(static templ.Attributes, parts ...templ.OrderedAttributes) templ.Attributes {
	out := make(templ.Attributes, len(static)+2*len(parts))
	for k, v := range static {
		out[k] = v
	}
	for _, p := range parts {
		for _, kv := range p {
			out[kv.Key] = kv.Value
		}
	}
	return out
}
