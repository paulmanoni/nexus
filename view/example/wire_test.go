package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// The board's live updates travel as the parts of its render tree that
// changed: adopting a pet sends that row's branch and the count, not the
// page's markup.
func TestBoardTravelsAsTreeChanges(t *testing.T) {
	srv := httptest.NewServer(boot(t))
	t.Cleanup(srv.Close)
	conn, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(srv.URL, "http")+"/board/_live", http.Header{"Origin": {srv.URL}})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	read := func() (map[string]any, int) {
		t.Helper()
		_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
		_, b, err := conn.ReadMessage()
		if err != nil {
			t.Fatal(err)
		}
		var m map[string]any
		_ = json.Unmarshal(b, &m)
		return m, len(b)
	}
	first, size := read()
	if first["full"] != true || size < 1000 {
		t.Fatalf("the first reply is the whole tree: %d bytes %v", size, first["full"])
	}
	// event sends an event and returns the change the broadcast pushes.
	event := func(ref int, name string) (string, int) {
		t.Helper()
		if err := conn.WriteJSON(map[string]any{"ref": ref, "event": name, "args": []any{"Biscuit"}}); err != nil {
			t.Fatal(err)
		}
		var push string
		var size int
		for gotReply := false; !gotReply || push == ""; {
			m, n := read()
			if m["tree"] == nil {
				continue
			}
			if m["full"] == true {
				t.Fatalf("%s sent the whole tree", name)
			}
			if m["ref"] != nil {
				gotReply = true
				continue
			}
			b, _ := json.Marshal(m["tree"])
			push, size = string(b), n
		}
		return push, size
	}
	event(1, "Adopt")
	event(2, "Return")
	// The rows' markup, statics and buttons are all held by now: adopting
	// again sends references and the values that changed.
	push, size := event(3, "Adopt")
	if size > 300 || strings.Contains(push, "\\u003cbutton") || strings.Contains(push, `"s"`) {
		t.Fatalf("adopting again sent %d bytes: %s", size, push)
	}
}
