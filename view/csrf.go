package view

import (
	"context"
	"html"
	"io"

	"github.com/a-h/templ"

	"github.com/paulmanoni/nexus/v2/middleware/secure"
)

// CSRF is the hidden field that carries the request's CSRF token in a form
// posted over plain HTTP — a sign-in page, a page without the view runtime:
//
//	<form method="post" action={ SignIn.URL(ctx) }>
//		@view.CSRF()
//		…
//	</form>
//
// It renders nothing when CSRF is off. A live page's forms need none: the
// runtime adds the token as they submit.
func CSRF() templ.Component {
	return templ.ComponentFunc(func(ctx context.Context, w io.Writer) error {
		field, token, ok := secure.CSRFToken(ctx)
		if !ok {
			return nil
		}
		_, err := io.WriteString(w, `<input type="hidden" name="`+html.EscapeString(field)+`" value="`+html.EscapeString(token)+`">`)
		return err
	})
}
