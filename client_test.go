package nexus

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"go.uber.org/fx"
	"go.uber.org/fx/fxtest"
)

// --- helpers ---

type getUserArgs struct {
	ID    string `json:"id"   uri:"id"`
	Trace string `json:"trace,omitempty" query:"trace"`
}

type createUserArgs struct {
	Name string `json:"name"`
	Age  int    `json:"age,omitempty"`
}

type userResp struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// --- helpers tests ---

func TestExpandPath(t *testing.T) {
	t.Run("simple", func(t *testing.T) {
		out, err := expandPath("/users/:id", getUserArgs{ID: "u42"})
		if err != nil {
			t.Fatal(err)
		}
		if out != "/users/u42" {
			t.Fatalf("got %q", out)
		}
	})
	t.Run("escapes", func(t *testing.T) {
		out, err := expandPath("/users/:id", getUserArgs{ID: "a/b"})
		if err != nil {
			t.Fatal(err)
		}
		if out != "/users/a%2Fb" {
			t.Fatalf("got %q", out)
		}
	})
	t.Run("missing tag fails loud", func(t *testing.T) {
		_, err := expandPath("/users/:id", struct{ X string }{X: "1"})
		if err == nil {
			t.Fatal("expected error for unsubstituted token")
		}
	})
	t.Run("no params returns verbatim", func(t *testing.T) {
		out, err := expandPath("/users", nil)
		if err != nil || out != "/users" {
			t.Fatalf("got %q, %v", out, err)
		}
	})
}

func TestMethodHasBody(t *testing.T) {
	for _, c := range []struct {
		m    string
		want bool
	}{
		{"GET", false}, {"DELETE", false}, {"HEAD", false},
		{"POST", true}, {"PUT", true}, {"PATCH", true},
		{"post", true}, // case-insensitive
	} {
		if got := methodHasBody(c.m); got != c.want {
			t.Errorf("methodHasBody(%q)=%v want %v", c.m, got, c.want)
		}
	}
}

func TestEncodeQuery(t *testing.T) {
	type args struct {
		Q     string   `query:"q"`
		Tags  []string `query:"tag"`
		Count int      `query:"count"`
		Empty string   `query:"empty"` // zero — should be omitted
		ID    string   `uri:"id"`      // path — should be skipped
	}
	got, err := encodeQuery(args{Q: "hello", Tags: []string{"a", "b"}, Count: 5, ID: "skip-me"})
	if err != nil {
		t.Fatal(err)
	}
	// url.Values.Encode sorts keys — output is deterministic.
	want := "count=5&q=hello&tag=a&tag=b"
	if got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}

func TestEncodeJSONBody_OmitsURIFields(t *testing.T) {
	type args struct {
		ID   string `uri:"id" json:"id"`
		Name string `json:"name"`
	}
	out, err := encodeJSONBody(args{ID: "u42", Name: "Alice"})
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	if _, has := got["id"]; has {
		t.Fatalf("uri field leaked into body: %s", out)
	}
	if got["name"] != "Alice" {
		t.Fatalf("body missing field: %s", out)
	}
}

// --- RemoteCaller tests ---

func TestRemoteCaller_GET(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" {
			t.Errorf("method: %s", r.Method)
		}
		if r.URL.Path != "/users/u42" {
			t.Errorf("path: %s", r.URL.Path)
		}
		if r.URL.Query().Get("trace") != "yes" {
			t.Errorf("query trace: %q", r.URL.Query().Get("trace"))
		}
		_ = json.NewEncoder(w).Encode(userResp{ID: "u42", Name: "Alice"})
	}))
	defer srv.Close()

	c := NewRemoteCaller(srv.URL)
	var out userResp
	err := c.Call(context.Background(), "GET", "/users/:id", getUserArgs{ID: "u42", Trace: "yes"}, &out)
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if !reflect.DeepEqual(out, userResp{ID: "u42", Name: "Alice"}) {
		t.Fatalf("decoded mismatch: %+v", out)
	}
}

func TestRemoteCaller_POST(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			t.Errorf("method: %s", r.Method)
		}
		if ct := r.Header.Get("Content-Type"); ct != "application/json" {
			t.Errorf("content-type: %q", ct)
		}
		body, _ := io.ReadAll(r.Body)
		var got createUserArgs
		_ = json.Unmarshal(body, &got)
		if got.Name != "Bob" || got.Age != 33 {
			t.Errorf("body: %+v", got)
		}
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(userResp{ID: "u43", Name: got.Name})
	}))
	defer srv.Close()

	c := NewRemoteCaller(srv.URL)
	var out userResp
	err := c.Call(context.Background(), "POST", "/users", createUserArgs{Name: "Bob", Age: 33}, &out)
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if out.ID != "u43" {
		t.Fatalf("got %+v", out)
	}
}

func TestRemoteCaller_NonOK_ReturnsRemoteError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"missing id"}`))
	}))
	defer srv.Close()

	c := NewRemoteCaller(srv.URL)
	err := c.Call(context.Background(), "GET", "/users/:id", getUserArgs{ID: "x"}, nil)
	if err == nil {
		t.Fatal("expected error")
	}
	var re *RemoteError
	if !errors.As(err, &re) {
		t.Fatalf("wrong error type: %T", err)
	}
	if re.Status != 400 {
		t.Fatalf("status: %d", re.Status)
	}
	if re.Message != "missing id" {
		t.Fatalf("message: %q", re.Message)
	}
}

func TestRemoteCaller_AuthPropagator(t *testing.T) {
	var seen string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = r.Header.Get("Authorization")
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	c := NewRemoteCaller(srv.URL)
	ctx := WithCallerAuthorization(context.Background(), "Bearer token-xyz")
	if err := c.Call(ctx, "GET", "/x", nil, nil); err != nil {
		t.Fatal(err)
	}
	if seen != "Bearer token-xyz" {
		t.Fatalf("Authorization not forwarded: %q", seen)
	}
}

// --- LocalInvoker tests ---

// localUserHandler is a minimal AsRest handler we point a LocalInvoker
// at — proves that arg binding, response encoding, and the registered
// middleware chain all run identically to a real HTTP call.
func localUserHandler(p Params[getUserArgs]) (*userResp, error) {
	if p.Args.ID == "" {
		return nil, errors.New("id required")
	}
	return &userResp{ID: p.Args.ID, Name: "Alice"}, nil
}

func TestLocalInvoker_RoundTrip(t *testing.T) {
	var app *App
	fxApp := fxtest.New(t,
		fxBootOptions(Config{Addr: "127.0.0.1:0"}),
		AsRest("GET", "/users/:id", localUserHandler).nexusOption(),
		fx.Populate(&app),
	)
	fxApp.RequireStart()
	defer fxApp.RequireStop()

	inv := NewLocalInvoker(app)
	var out userResp
	err := inv.Invoke(context.Background(), "GET", "/users/:id",
		getUserArgs{ID: "u42"}, &out)
	if err != nil {
		t.Fatalf("invoke: %v", err)
	}
	if out.ID != "u42" {
		t.Fatalf("decoded mismatch: %+v", out)
	}
}

func TestLocalInvoker_HandlerError(t *testing.T) {
	failHandler := func(p Params[struct{}]) (*userResp, error) {
		return nil, errors.New("boom")
	}
	var app *App
	fxApp := fxtest.New(t,
		fxBootOptions(Config{Addr: "127.0.0.1:0"}),
		AsRest("POST", "/fail", failHandler).nexusOption(),
		fx.Populate(&app),
	)
	fxApp.RequireStart()
	defer fxApp.RequireStop()

	inv := NewLocalInvoker(app)
	err := inv.Invoke(context.Background(), "POST", "/fail", struct{}{}, nil)
	if err == nil {
		t.Fatal("expected error")
	}
	var re *RemoteError
	if !errors.As(err, &re) {
		t.Fatalf("wrong error type: %T (%v)", err, err)
	}
	if re.Status < 400 {
		t.Fatalf("expected non-2xx, got %d", re.Status)
	}
	if !strings.Contains(re.Message, "boom") && !strings.Contains(string(re.RawBody), "boom") {
		t.Fatalf("error didn't surface handler message: msg=%q body=%s", re.Message, re.RawBody)
	}
}