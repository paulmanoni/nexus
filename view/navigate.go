package view

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"github.com/paulmanoni/nexus/v2"
)

// Navigation between live pages happens on the page's socket, as Phoenix
// LiveView's does. A view.Link (or view.PushPatch / view.PushNavigate from an
// event) sends the browser's new URL over the socket:
//
//	the same page, another query     a patch: the page's Params runs and the
//	                                 reply is the change to its tree
//	another live page                the connection is handed to that page's
//	                                 socket route — its gates, DI and path
//	                                 parameters as for a page load — which
//	                                 mounts and sends its tree; statics the
//	                                 connection holds (a shared layout's) are
//	                                 not sent again
//	anything else, or a refusal      the browser loads the URL over HTTP

// liveConn is a browser's live connection: it outlives the pages it serves.
type liveConn struct {
	conn   *websocket.Conn
	events chan liveEvent // what the browser sends, for the connection's life
	tree   treeDiffer     // the tree the browser holds and the statics it has
	// trimmed: the tree was let go while idle (LiveIdleTrim); the next reply
	// tells the browser to drop its statics too.
	trimmed bool
	req     *http.Request // the upgrade request, to open the next page as

	next    string                     // the URL of a page to hand the connection to
	target  string                     // the URL the page being opened was navigated to
	pending func(base context.Context) // the page a hand-off opened, to run
}

type liveConnKey struct{}

func newLiveConn(conn *websocket.Conn, r *http.Request) *liveConn {
	// Only what a hand-off reuses is kept of the upgrade request.
	kept := &http.Request{Header: r.Header.Clone(), RemoteAddr: r.RemoteAddr, Host: r.Host}
	lc := &liveConn{conn: conn, events: make(chan liveEvent), req: kept}
	go func() {
		defer close(lc.events)
		for {
			var ev liveEvent
			if err := conn.ReadJSON(&ev); err != nil {
				return
			}
			_ = conn.SetReadDeadline(time.Now().Add(livePongWait))
			lc.events <- ev
		}
	}()
	return lc
}

// handoff opens the page at target on this connection, through the app's
// router as a request for its socket route: it reports whether a live page
// took the connection.
func (lc *liveConn) handoff(ctx context.Context, h http.Handler, target string) bool {
	u, err := url.Parse(target)
	if err != nil || u.IsAbs() || u.Host != "" || !strings.HasPrefix(u.Path, "/") || h == nil {
		return false
	}
	req, err := http.NewRequestWithContext(context.WithValue(ctx, liveConnKey{}, lc), http.MethodGet, strings.TrimSuffix(u.Path, "/")+"/_live", nil)
	if err != nil {
		return false
	}
	req.Header = lc.req.Header.Clone()
	req.Header.Del("Upgrade")
	req.Header.Del("Connection")
	req.RemoteAddr, req.Host = lc.req.RemoteAddr, lc.req.Host
	lc.target, lc.pending = u.RequestURI(), nil
	h.ServeHTTP(discard{header: http.Header{}}, req)
	return lc.pending != nil
}

// discard is the response of a hand-off a page refused: the browser loads
// the URL itself and gets the answer then.
type discard struct{ header http.Header }

func (d discard) Header() http.Header       { return d.header }
func (discard) Write(b []byte) (int, error) { return len(b), nil }
func (discard) WriteHeader(int)             {}

// navKey is where an event finds the navigation it may ask for.
type navKey struct{}

// PushPatch, from a live page's event, moves the browser to href once the
// event's reply is sent: on the same page (another query), the page's Params
// runs and the page is patched; another live page opens on the connection.
// The browser's history gets the new URL.
//
//	func (p *Orders) Filter(ctx context.Context, status string) error {
//	    view.PushPatch(ctx, "/orders?status="+url.QueryEscape(status))
//	    return nil
//	}
func PushPatch(ctx context.Context, href string) { pushNav(ctx, href) }

// PushNavigate, from a live page's event, opens the page at href — on the
// connection when it is a live page, else with a page load.
func PushNavigate(ctx context.Context, href string) { pushNav(ctx, href) }

func pushNav(ctx context.Context, href string) {
	if p, ok := ctx.Value(navKey{}).(*string); ok {
		*p = href
	}
}

// pageURL is the URL a page shows: the browser's, when it says so and it is
// this page's.
func pageURL(page, sent string) *url.URL {
	if u, err := url.Parse(sent); err == nil && strings.TrimSuffix(u.Path, "/") == strings.TrimSuffix(page, "/") && !u.IsAbs() {
		return u
	}
	return &url.URL{Path: page}
}

// live tracks each app's open live connections, so the app's stop closes
// them: a page outlives the request that opened it.
var live = struct {
	sync.Mutex
	m map[*nexus.App]map[*websocket.Conn]context.CancelFunc
}{m: map[*nexus.App]map[*websocket.Conn]context.CancelFunc{}}

func track(app *nexus.App, conn *websocket.Conn, stop context.CancelFunc) (untrack func()) {
	live.Lock()
	defer live.Unlock()
	if live.m[app] == nil {
		live.m[app] = map[*websocket.Conn]context.CancelFunc{}
	}
	live.m[app][conn] = stop
	return func() {
		live.Lock()
		defer live.Unlock()
		delete(live.m[app], conn)
		if len(live.m[app]) == 0 {
			delete(live.m, app)
		}
	}
}

// closeLive ends every live connection of app.
func closeLive(app *nexus.App) {
	live.Lock()
	conns := live.m[app]
	delete(live.m, app)
	live.Unlock()
	for conn, stop := range conns {
		stop()
		_ = conn.Close()
	}
}
