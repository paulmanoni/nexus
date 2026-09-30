package view

import (
	"context"
	"io"

	"github.com/a-h/templ"
)

// Link is an in-app link: following it replaces the page without reloading
// the document — the new page is fetched, patched into the current one,
// live pages connect and disconnect as needed, and the address bar and the
// back button work as usual. A modified click (a new tab) or a link to
// another site behaves as a plain link.
//
//	@view.Link("/board") {
//	    Adoption board
//	}
//	@view.Link("/pets", templ.Attributes{"class": "underline"}) {
//	    Pets
//	}
//
// The href is sanitized like templ's own URLs (no javascript:).
func Link(href string, attrs ...templ.Attributes) templ.Component {
	return templ.ComponentFunc(func(ctx context.Context, w io.Writer) error {
		if _, err := io.WriteString(w, `<a href="`+templ.EscapeString(string(templ.URL(href)))+`" data-nx-nav`); err != nil {
			return err
		}
		for _, a := range attrs {
			if err := templ.RenderAttributes(ctx, w, a); err != nil {
				return err
			}
		}
		if _, err := io.WriteString(w, ">"); err != nil {
			return err
		}
		if children := templ.GetChildren(ctx); children != nil {
			if err := children.Render(templ.ClearChildren(ctx), w); err != nil {
				return err
			}
		}
		_, err := io.WriteString(w, "</a>")
		return err
	})
}
