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
