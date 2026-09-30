package view

import (
	"io"
	"net/http"
	"regexp"
	"strings"
	"testing"
)

var joinAttr = regexp.MustCompile(`data-nx-live-join="([0-9a-f]{32})"`)

func loadJoin(t *testing.T, url string) string {
	t.Helper()
	res, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(res.Body)
	m := joinAttr.FindStringSubmatch(string(b))
	if m == nil {
		t.Fatalf("no join id in the page:\n%s", b)
	}
	return m[1]
}

// The page's first render came over HTTP: a socket joining it sends
// nothing until something changes, and that change carries the render the
// browser will patch against.
func TestLiveJoinSendsNothingFirst(t *testing.T) {
	srv := bootLikes(t)
	join := loadJoin(t, srv.URL+"/likes/lobby")
	c := dialLive(t, srv, "/likes/lobby/_live?join="+join)

	// Messages arrive in order: if connecting had sent anything, it would
	// come before the reply to this event.
	if err := c.WriteJSON(liveEvent{Ref: 1, Event: "Like"}); err != nil {
		t.Fatal(err)
	}
	// Like changes the page through the broadcast (Info), so its own reply
	// is the unchanged render — in full, the base for later patches.
	first := reply(t, c)
	if first.Ref != 1 || first.Patch != nil || !strings.Contains(first.HTML, `<p id="likes">0</p>`) {
		t.Fatalf("the first reply must carry the full render: %+v", first)
	}
	if second := reply(t, c); second.Patch == nil || !strings.Contains(second.HTML, `<p id="likes">1</p>`) || !strings.Contains(second.HTML, "likes/liked") {
		t.Fatalf("later changes travel as patches: %+v", second)
	}

	// A join is good once: a second connection naming it gets a full render.
	again := dialLive(t, srv, "/likes/lobby/_live?join="+join)
	if r := reply(t, again); r.HTML == "" || r.Patch != nil {
		t.Fatalf("a used join = %+v", r)
	}
}

// A Mount that renders differently once connected corrects the page at once.
func TestLiveJoinCorrectsTheHTTPRender(t *testing.T) {
	srv := bootLive(t)
	join := loadJoin(t, srv.URL+"/count/ana")
	c := dialLive(t, srv, "/count/ana/_live?join="+join)
	if r := reply(t, c); !strings.Contains(r.HTML, `<p id="m">live</p>`) {
		t.Fatalf("the connected render differs from the HTTP one and must be sent: %+v", r)
	}
}

// A reconnect (no join) mounts afresh and sends the full render.
func TestLiveReconnect(t *testing.T) {
	srv := bootLikes(t)
	c := dialLive(t, srv, "/likes/lobby/_live")
	reply(t, c)
	if err := c.WriteJSON(liveEvent{Event: "Like"}); err != nil {
		t.Fatal(err)
	}
	reply(t, c)
	c.Close()

	again := dialLive(t, srv, "/likes/lobby/_live")
	if r := reply(t, again); r.HTML == "" || !strings.Contains(r.HTML, `<p id="likes">1</p>`) {
		t.Fatalf("a reconnect = %+v", r)
	}
}
