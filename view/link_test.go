package view

import (
	"bytes"
	"context"
	"io"
	"testing"

	"github.com/a-h/templ"
)

func TestLink(t *testing.T) {
	text := templ.ComponentFunc(func(ctx context.Context, w io.Writer) error {
		_, err := io.WriteString(w, "Board &amp; more")
		return err
	})
	for href, want := range map[string]string{
		"/board?tab=a&b=1":    `<a href="/board?tab=a&amp;b=1" data-nx-nav class="underline">Board &amp; more</a>`,
		"javascript:alert(1)": `<a href="about:invalid#TemplFailedSanitizationURL" data-nx-nav class="underline">Board &amp; more</a>`,
	} {
		var buf bytes.Buffer
		ctx := templ.WithChildren(context.Background(), text)
		if err := Link(href, templ.Attributes{"class": "underline"}).Render(ctx, &buf); err != nil {
			t.Fatal(err)
		}
		if buf.String() != want {
			t.Errorf("Link(%q)\n got %s\nwant %s", href, buf.String(), want)
		}
	}
}
