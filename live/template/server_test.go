package template

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// httpReg builds a fresh engine + registers Counter + returns an
// httptest server bound to GET /live. Tests use the returned base
// URL to drive both the SSR path and the WS upgrade.
func httpReg(t *testing.T) (*httptest.Server, *Engine) {
	t.Helper()
	e := New()
	if err := e.Register("Counter", []byte(counterTmpl), func() Component { return &counterComponent{} }); err != nil {
		t.Fatalf("register: %v", err)
	}
	srv := httptest.NewServer(e.Handler("Counter"))
	t.Cleanup(func() { srv.Close() })
	return srv, e
}

// --- SSR path -------------------------------------------------------

func TestServer_SSR_ReturnsRenderedHTML(t *testing.T) {
	srv, _ := httpReg(t)

	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Errorf("Content-Type = %q", ct)
	}

	buf := make([]byte, 4096)
	n, _ := resp.Body.Read(buf)
	body := string(buf[:n])
	for _, want := range []string{
		`<title>Counter</title>`,
		`data-nl-component="Counter"`,
		`<span id="count">0</span>`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("SSR body missing %q:\n%s", want, body)
		}
	}
}

func TestServer_SSR_IncludesClientScriptTag(t *testing.T) {
	srv, _ := httpReg(t)
	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()
	buf := make([]byte, 4096)
	n, _ := resp.Body.Read(buf)
	body := string(buf[:n])
	if !strings.Contains(body, `<script src="`+ScriptPath+`" defer></script>`) {
		t.Errorf("SSR body missing client script tag; got:\n%s", body)
	}
}

func TestServer_Script_ServesJS(t *testing.T) {
	e := New()
	srv := httptest.NewServer(e.Script())
	defer srv.Close()

	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/javascript") {
		t.Errorf("Content-Type = %q", ct)
	}
	buf := make([]byte, 16384)
	n, _ := resp.Body.Read(buf)
	body := string(buf[:n])
	// Sanity check a few load-bearing identifiers from the JS so a
	// missing/mistruncated embed is caught.
	for _, want := range []string{
		"data-nl-component",
		"WebSocket",
		"applyDiff",
		"morphChildren",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("script body missing %q", want)
		}
	}
}

func TestServer_UnknownComponent_Returns404(t *testing.T) {
	e := New()
	srv := httptest.NewServer(e.Handler("Missing"))
	defer srv.Close()

	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d want 404", resp.StatusCode)
	}
}

// --- WS path --------------------------------------------------------

// dialWS opens a websocket against the test server, doing the
// http:// → ws:// scheme swap that gorilla expects.
func dialWS(t *testing.T, srv *httptest.Server) *websocket.Conn {
	t.Helper()
	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http")
	conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

func readFrame(t *testing.T, conn *websocket.Conn) Outbound {
	t.Helper()
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	_, raw, err := conn.ReadMessage()
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var msg Outbound
	if err := json.Unmarshal(raw, &msg); err != nil {
		t.Fatalf("unmarshal %q: %v", raw, err)
	}
	return msg
}

func writeFrame(t *testing.T, conn *websocket.Conn, msg Inbound) {
	t.Helper()
	raw, err := json.Marshal(msg)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := conn.WriteMessage(websocket.TextMessage, raw); err != nil {
		t.Fatalf("write: %v", err)
	}
}

func TestServer_WS_JoinedFirstFrame(t *testing.T) {
	srv, _ := httpReg(t)
	conn := dialWS(t, srv)

	msg := readFrame(t, conn)
	if msg.Type != "joined" {
		t.Fatalf("expected joined; got %+v", msg)
	}
	if msg.Rendered == nil {
		t.Fatal("missing Rendered")
	}
	if got := msg.Rendered.HTML(); got != `<span id="count">0</span>` {
		t.Errorf("initial HTML = %q", got)
	}
}

func TestServer_WS_EventRoundTrip(t *testing.T) {
	srv, _ := httpReg(t)
	conn := dialWS(t, srv)

	_ = readFrame(t, conn) // joined
	writeFrame(t, conn, Inbound{Type: "event", Name: "inc"})

	msg := readFrame(t, conn)
	if msg.Type != "diff" {
		t.Fatalf("expected diff; got %+v", msg)
	}
	if got := msg.Diff["0"]; got != "1" {
		t.Errorf("diff[\"0\"] = %#v want \"1\"", got)
	}
}

func TestServer_WS_PingPong(t *testing.T) {
	srv, _ := httpReg(t)
	conn := dialWS(t, srv)

	_ = readFrame(t, conn) // joined
	writeFrame(t, conn, Inbound{Type: "ping"})
	if msg := readFrame(t, conn); msg.Type != "pong" {
		t.Errorf("got %+v", msg)
	}
}

func TestServer_WS_PayloadDecoded(t *testing.T) {
	srv, _ := httpReg(t)
	conn := dialWS(t, srv)
	_ = readFrame(t, conn) // joined

	writeFrame(t, conn, Inbound{Type: "event", Name: "add", Payload: Payload{"by": 7}})
	msg := readFrame(t, conn)
	if msg.Type != "diff" {
		t.Fatalf("got %+v", msg)
	}
	if got := msg.Diff["0"]; got != "7" {
		t.Errorf("diff[\"0\"] = %#v want \"7\"", got)
	}
}

func TestServer_WS_TwoEventsTwoDiffs(t *testing.T) {
	srv, _ := httpReg(t)
	conn := dialWS(t, srv)
	_ = readFrame(t, conn) // joined

	writeFrame(t, conn, Inbound{Type: "event", Name: "inc"})
	m1 := readFrame(t, conn)
	writeFrame(t, conn, Inbound{Type: "event", Name: "inc"})
	m2 := readFrame(t, conn)

	if m1.Type != "diff" || m2.Type != "diff" {
		t.Fatalf("expected two diffs; got %+v / %+v", m1, m2)
	}
	if m1.Diff["0"] != "1" || m2.Diff["0"] != "2" {
		t.Errorf("counts: m1=%v m2=%v", m1.Diff["0"], m2.Diff["0"])
	}
}
