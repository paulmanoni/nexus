package viewtest_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/a-h/templ"

	"github.com/paulmanoni/nexus/v2"
	"github.com/paulmanoni/nexus/v2/config"
	"github.com/paulmanoni/nexus/v2/nexustest"
	"github.com/paulmanoni/nexus/v2/view"
	"github.com/paulmanoni/nexus/v2/view/viewtest"
)

// Inbox is a service a context processor depends on.
type Inbox struct {
	calls, unread int
	fail          bool
}

// Layout is what every page's layout shows, from a context processor.
type Layout struct {
	User   string
	Unread int
}

type missing struct{}

func layoutProcessor(ctx context.Context, inbox *Inbox) (Layout, error) {
	inbox.calls++
	if inbox.fail {
		return Layout{}, errors.New("inbox down")
	}
	return Layout{User: "ann", Unread: inbox.unread}, nil
}

// header and footer both read the Layout: one render computes it once.
func header() templ.Component {
	return templ.ComponentFunc(func(ctx context.Context, w io.Writer) error {
		l := view.FromContext[Layout](ctx)
		_, err := fmt.Fprintf(w, `<header id="user">%s</header>`, l.User)
		return err
	})
}

func footer() templ.Component {
	return templ.ComponentFunc(func(ctx context.Context, w io.Writer) error {
		_, err := fmt.Fprintf(w, `<footer id="unread">%d</footer>`, view.FromContext[Layout](ctx).Unread)
		return err
	})
}

func layoutHome() templ.Component {
	return templ.ComponentFunc(func(ctx context.Context, w io.Writer) error {
		io.WriteString(w, `<!DOCTYPE html><html><head><title>Home</title></head><body>`)
		header().Render(ctx, w)
		footer().Render(ctx, w)
		_, err := io.WriteString(w, `</body></html>`)
		return err
	})
}

func lost() templ.Component {
	return templ.ComponentFunc(func(ctx context.Context, w io.Writer) error {
		view.FromContext[missing](ctx)
		return nil
	})
}

// Mailbox is a live page whose layout follows the processor on each render.
type Mailbox struct{ view.LiveView }

func (m *Mailbox) Arrive(ctx context.Context, inbox *Inbox) error { inbox.unread++; return nil }

func (m *Mailbox) Render() templ.Component {
	return templ.ComponentFunc(func(ctx context.Context, w io.Writer) error {
		io.WriteString(w, `<!DOCTYPE html><html><head><title>Mail</title>`)
		view.Script().Render(ctx, w)
		io.WriteString(w, `</head><body>`)
		footer().Render(ctx, w)
		_, err := fmt.Fprintf(w, `<button id="arrive" onclick="%s">arrive</button></body></html>`, view.Send(m.Arrive).Call)
		return err
	})
}

func TestContextProcessor(t *testing.T) {
	inbox := &Inbox{unread: 3}
	app := nexustest.New(t, config.Runtime{},
		nexus.Supply(inbox),
		view.ContextProcessor(layoutProcessor),
		view.Page("GET", "/home", layoutHome),
		view.Page("GET", "/lost", lost),
		view.Live[*Mailbox]("/mail"),
	)

	p := viewtest.Get(t, app, "/home")
	p.Expect("#user").Text("ann")
	p.Expect("#unread").Text("3")
	if inbox.calls != 1 {
		t.Errorf("one render ran the processor %d times", inbox.calls)
	}
	viewtest.Get(t, app, "/home")
	if inbox.calls != 2 {
		t.Errorf("a second request reuses the first's value: %d calls", inbox.calls)
	}

	// A live page reads it on every render.
	m := viewtest.Mount[*Mailbox](t, app)
	m.Expect("#unread").Text("3")
	m.Click("#arrive").Wait()
	m.Expect("#unread").Text("4")

	status := func(path string) int {
		rec := httptest.NewRecorder()
		app.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		return rec.Code
	}
	inbox.fail = true
	if c := status("/home"); c != http.StatusInternalServerError {
		t.Errorf("a failing processor: %d", c)
	}
	inbox.fail = false
	if c := status("/lost"); c != http.StatusInternalServerError {
		t.Errorf("a type no processor returns: %d", c)
	}
}

func TestContextProcessorShape(t *testing.T) {
	_, _, err := nexus.InProcess(config.Runtime{}, view.ContextProcessor(func(n int) string { return "" }))
	if err == nil || !strings.Contains(err.Error(), "func(ctx context.Context") {
		t.Errorf("a processor without ctx: %v", err)
	}
}
