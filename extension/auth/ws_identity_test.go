package auth_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/paulmanoni/nexus/v2"
	"github.com/paulmanoni/nexus/v2/config"
	"github.com/paulmanoni/nexus/v2/extension/auth"
	"github.com/paulmanoni/nexus/v2/extension/auth/authtest"
)

type pokeArgs struct {
	To string `json:"to"`
}

// A WebSocket connection belongs to the identity auth resolved for its
// upgrade request, so EmitToUser reaches the token's owner — and a socket
// that only claims the id (?userId=, the authenticate message) gets nothing.
func TestWebSocketIdentityComesFromAuth(t *testing.T) {
	app, stop, err := nexus.InProcess(config.Runtime{},
		auth.Module(auth.Config{Users: auth.StaticUsers(authtest.NewUsers())}),
		nexus.AsWS("/live", "poke", func(sess *nexus.WSSession, p nexus.Params[pokeArgs]) error {
			sess.EmitToUser("private", map[string]string{"to": p.Args.To}, p.Args.To)
			return nil
		}, auth.Required()),
	)
	if err != nil {
		t.Fatalf("InProcess: %v", err)
	}
	defer stop(context.Background())
	ts := httptest.NewServer(app)
	defer ts.Close()
	base := "ws" + strings.TrimPrefix(ts.URL, "http") + "/live"

	dial := func(query, token string) *websocket.Conn {
		t.Helper()
		c, resp, err := websocket.DefaultDialer.Dial(base+query, wsHeader(token))
		if err != nil {
			code := 0
			if resp != nil {
				code = resp.StatusCode
			}
			t.Fatalf("dial %s: %v (HTTP %d)", query, err, code)
		}
		c.SetReadDeadline(time.Now().Add(2 * time.Second))
		if _, _, err := c.ReadMessage(); err != nil {
			t.Fatalf("greeting: %v", err)
		}
		return c
	}
	alice := dial("", "tok-alice")
	defer alice.Close()
	bob := dial("?userId=alice", "tok-bob")
	defer bob.Close()
	if err := bob.WriteJSON(map[string]any{"type": "authenticate", "userId": "alice"}); err != nil {
		t.Fatal(err)
	}
	bob.SetReadDeadline(time.Now().Add(2 * time.Second))
	_, raw, err := bob.ReadMessage()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"userId":"bob"`) {
		t.Fatalf("bob's authenticate reply = %s, want his own identity", raw)
	}

	if err := bob.WriteJSON(map[string]any{"type": "poke", "data": map[string]string{"to": "alice"}}); err != nil {
		t.Fatal(err)
	}
	alice.SetReadDeadline(time.Now().Add(2 * time.Second))
	_, raw, err = alice.ReadMessage()
	if err != nil {
		t.Fatalf("alice got nothing: %v", err)
	}
	var ev struct{ Type string }
	_ = json.Unmarshal(raw, &ev)
	if ev.Type != "private" {
		t.Fatalf("alice got %s, want her private event", raw)
	}
	bob.SetReadDeadline(time.Now().Add(300 * time.Millisecond))
	if _, raw, err := bob.ReadMessage(); err == nil {
		t.Fatalf("bob, claiming alice, received %s", raw)
	}

	if _, resp, err := websocket.DefaultDialer.Dial(base+"?userId=alice", nil); err == nil || resp == nil || resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("an unauthenticated upgrade must be refused by auth.Required (err %v)", err)
	}
}

// wsHeader authenticates a test dial: tok-alice holds "watch", tok-bob
// doesn't; anything else is anonymous.
func wsHeader(token string) http.Header {
	h := http.Header{}
	switch token {
	case "tok-alice":
		h = authtest.As(&auth.Identity{ID: "alice", Perms: []string{"watch"}})
	case "tok-bob":
		h = authtest.As(&auth.Identity{ID: "bob"})
	}
	return h
}

// A WS handler's context carries the upgrade request's identity and auth
// state, so auth.Current and auth.Can work there as in REST handlers.
func TestWebSocketHandlerContextCarriesAuth(t *testing.T) {
	app, stop, err := nexus.InProcess(config.Runtime{},
		auth.Module(auth.Config{Users: auth.StaticUsers(authtest.NewUsers())}),
		nexus.AsWS("/live", "whoami", func(sess *nexus.WSSession, p nexus.Params[struct{}]) error {
			id := auth.Current(p.Context)
			who := ""
			if id != nil {
				who = id.ID
			}
			return sess.Send("me", map[string]any{"id": who, "canWatch": auth.Can(p.Context, "watch")})
		}, auth.Required()),
	)
	if err != nil {
		t.Fatalf("InProcess: %v", err)
	}
	defer stop(context.Background())
	ts := httptest.NewServer(app)
	defer ts.Close()
	base := "ws" + strings.TrimPrefix(ts.URL, "http") + "/live"

	for _, c := range []struct {
		token, id string
		can       bool
	}{{"tok-alice", "alice", true}, {"tok-bob", "bob", false}} {
		conn, _, err := websocket.DefaultDialer.Dial(base, wsHeader(c.token))
		if err != nil {
			t.Fatalf("dial %s: %v", c.token, err)
		}
		conn.SetReadDeadline(time.Now().Add(2 * time.Second))
		if _, _, err := conn.ReadMessage(); err != nil {
			t.Fatalf("greeting: %v", err)
		}
		// Two messages: the context must hold across the connection, not just the first.
		for i := 0; i < 2; i++ {
			if err := conn.WriteJSON(map[string]any{"type": "whoami"}); err != nil {
				t.Fatal(err)
			}
			_, raw, err := conn.ReadMessage()
			if err != nil {
				t.Fatalf("%s: %v", c.token, err)
			}
			var ev struct {
				Data struct {
					ID       string `json:"id"`
					CanWatch bool   `json:"canWatch"`
				} `json:"data"`
			}
			_ = json.Unmarshal(raw, &ev)
			if ev.Data.ID != c.id || ev.Data.CanWatch != c.can {
				t.Fatalf("%s message %d: handler saw %s, want id %q canWatch %v", c.token, i, raw, c.id, c.can)
			}
		}
		conn.Close()
	}
}
