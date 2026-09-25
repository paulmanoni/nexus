package auth_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/paulmanoni/nexus"
	"github.com/paulmanoni/nexus/extension/auth"
)

type pokeArgs struct {
	To string `json:"to"`
}

// A WebSocket connection belongs to the identity auth resolved for its
// upgrade request, so EmitToUser reaches the token's owner — and a socket
// that only claims the id (?userId=, the authenticate message) gets nothing.
func TestWebSocketIdentityComesFromAuth(t *testing.T) {
	tokens := map[string]string{"tok-alice": "alice", "tok-bob": "bob"}
	app, stop, err := nexus.InProcess(nexus.Config{},
		auth.Single(func(_ context.Context, tok string) (*auth.Identity, error) {
			if id, ok := tokens[tok]; ok {
				return &auth.Identity{ID: id}, nil
			}
			return nil, errors.New("unknown token")
		}),
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
		c, resp, err := websocket.DefaultDialer.Dial(base+query, http.Header{"Authorization": {"Bearer " + token}})
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

// wsAuthBackend resolves tokens and grants "watch" by name, not by role —
// so auth.Can only answers true when the module's state reached the handler.
type wsAuthBackend struct{}

func (wsAuthBackend) Resolve(_ context.Context, tok string) (*auth.Identity, error) {
	switch tok {
	case "tok-alice":
		return &auth.Identity{ID: "alice"}, nil
	case "tok-bob":
		return &auth.Identity{ID: "bob"}, nil
	}
	return nil, errors.New("unknown token")
}

func (wsAuthBackend) Authorize(id *auth.Identity, required []string) bool {
	return id != nil && id.ID == "alice"
}

// A WS handler's context carries the upgrade request's identity and auth
// state, so auth.IdentityFrom and auth.Can work there as in REST handlers.
func TestWebSocketHandlerContextCarriesAuth(t *testing.T) {
	app, stop, err := nexus.InProcess(nexus.Config{},
		auth.Module(auth.Config{
			Authentication: auth.Authentication{Schemes: []auth.Scheme{{Extract: auth.Bearer()}}},
			Backend:        auth.StaticBackend(wsAuthBackend{}),
		}),
		nexus.AsWS("/live", "whoami", func(sess *nexus.WSSession, p nexus.Params[struct{}]) error {
			id, _ := auth.IdentityFrom(p.Context)
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
		conn, _, err := websocket.DefaultDialer.Dial(base, http.Header{"Authorization": {"Bearer " + c.token}})
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
