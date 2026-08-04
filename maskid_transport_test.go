package nexus

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	"github.com/paulmanoni/nexus/internal/maskhook"
)

// The transport wiring is what's under test here, not the cipher, so the
// codec is a visible reversible stand-in: 41 <-> "mask-41". A real codec
// lives in extension/maskid, which can't be imported from this package
// without a cycle.
func installTestMask(t *testing.T) {
	t.Helper()
	maskhook.Install(maskhook.Hooks{
		IsID: func(key string) bool { return key == "id" || key == "ownerId" },
		Mask: func(_ string, n int64) (string, bool) {
			return "mask-" + strconv.FormatInt(n, 10), true
		},
		Unmask: func(_, s string) (int64, bool) {
			if len(s) < 5 || s[:5] != "mask-" {
				return 0, false
			}
			n, err := strconv.ParseInt(s[5:], 10, 64)
			return n, err == nil
		},
	})
	t.Cleanup(maskhook.Uninstall)
}

type maskItem struct {
	ID      int    `json:"id"`
	OwnerID int    `json:"ownerId"`
	Count   int    `json:"count"`
	Title   string `json:"title"`
}

type maskGetArgs struct {
	ID int `path:"id"`
}

type maskCreateArgs struct {
	OwnerID int    `json:"ownerId"`
	Title   string `json:"title"`
}

type maskSearchArgs struct {
	OwnerID int `query:"ownerId"`
}

// seen records what the handlers actually received, so the assertions can
// distinguish "the response looked right" from "the handler got integers".
type maskSeen struct{ get, create, search int }

func maskTestApp(t *testing.T, seen *maskSeen) *httptest.Server {
	t.Helper()
	mod := Module("maskid_transport",
		Provide(func(app *App) *Service { return app.Service("items") }),
		AsRest("GET", "/items/:id", func(_ *Service, p Params[maskGetArgs]) (*maskItem, error) {
			seen.get = p.Args.ID
			return &maskItem{ID: p.Args.ID, OwnerID: 7, Count: 3, Title: "hello"}, nil
		}),
		AsRest("POST", "/items", func(_ *Service, p Params[maskCreateArgs]) (*maskItem, error) {
			seen.create = p.Args.OwnerID
			return &maskItem{ID: 99, OwnerID: p.Args.OwnerID, Title: p.Args.Title}, nil
		}),
		AsRest("GET", "/items", func(_ *Service, p Params[maskSearchArgs]) ([]maskItem, error) {
			seen.search = p.Args.OwnerID
			return []maskItem{{ID: 41, OwnerID: p.Args.OwnerID}}, nil
		}),
	)
	app, err := newApp(Config{Server: ServerConfig{Addr: "127.0.0.1:0"}}, mod)
	if err != nil {
		t.Fatalf("newApp: %v", err)
	}
	srv := httptest.NewServer(app.Engine())
	t.Cleanup(func() {
		srv.Close()
		app.Stop()
	})
	return srv
}

func decodeBody(t *testing.T, resp *http.Response) map[string]any {
	t.Helper()
	defer resp.Body.Close()
	blob, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode >= 300 {
		t.Fatalf("status %d: %s", resp.StatusCode, blob)
	}
	var out map[string]any
	if err := json.Unmarshal(blob, &out); err != nil {
		t.Fatalf("decode %s: %v", blob, err)
	}
	return out
}

// A masked path param must reach the handler as the integer it encodes,
// and the response must go back out masked.
func TestMaskID_RestPathParamRoundTrip(t *testing.T) {
	installTestMask(t)
	var seen maskSeen
	srv := maskTestApp(t, &seen)

	resp, err := http.Get(srv.URL + "/items/mask-41")
	if err != nil {
		t.Fatal(err)
	}
	got := decodeBody(t, resp)

	if seen.get != 41 {
		t.Errorf("handler received id %d, want 41", seen.get)
	}
	if got["id"] != "mask-41" {
		t.Errorf("response id = %#v, want \"mask-41\"", got["id"])
	}
	if got["ownerId"] != "mask-7" {
		t.Errorf("response ownerId = %#v, want \"mask-7\"", got["ownerId"])
	}
	// Fields the policy doesn't claim must be untouched — a masked
	// "count" would be a silent data corruption.
	if got["count"] != float64(3) || got["title"] != "hello" {
		t.Errorf("non-ID fields were rewritten: count=%#v title=%#v", got["count"], got["title"])
	}
}

func TestMaskID_RestJSONBodyRoundTrip(t *testing.T) {
	installTestMask(t)
	var seen maskSeen
	srv := maskTestApp(t, &seen)

	body := bytes.NewBufferString(`{"ownerId":"mask-7","title":"hi"}`)
	resp, err := http.Post(srv.URL+"/items", "application/json", body)
	if err != nil {
		t.Fatal(err)
	}
	got := decodeBody(t, resp)

	if seen.create != 7 {
		t.Errorf("handler received ownerId %d, want 7", seen.create)
	}
	if got["ownerId"] != "mask-7" || got["id"] != "mask-99" {
		t.Errorf("response not masked: %#v", got)
	}
	if got["title"] != "hi" {
		t.Errorf("title = %#v, want \"hi\"", got["title"])
	}
}

func TestMaskID_RestQueryParam(t *testing.T) {
	installTestMask(t)
	var seen maskSeen
	srv := maskTestApp(t, &seen)

	resp, err := http.Get(srv.URL + "/items?ownerId=mask-7")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	blob, _ := io.ReadAll(resp.Body)
	if seen.search != 7 {
		t.Errorf("handler received ownerId %d, want 7 (body %s)", seen.search, blob)
	}
	var rows []map[string]any
	if err := json.Unmarshal(blob, &rows); err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0]["id"] != "mask-41" {
		t.Errorf("list response not masked: %s", blob)
	}
}

// Nothing about an app that never installs the hook should change — this
// is the regression guard for the framework edits themselves.
func TestMaskID_DisabledLeavesTransportsUntouched(t *testing.T) {
	maskhook.Uninstall()
	var seen maskSeen
	srv := maskTestApp(t, &seen)

	resp, err := http.Get(srv.URL + "/items/41")
	if err != nil {
		t.Fatal(err)
	}
	got := decodeBody(t, resp)

	if seen.get != 41 {
		t.Errorf("handler received id %d, want 41", seen.get)
	}
	if got["id"] != float64(41) || got["ownerId"] != float64(7) {
		t.Errorf("ids were rewritten with masking off: %#v", got)
	}
}

type maskGQLArgs struct {
	ID int `graphql:"id,required"`
}

// A package-level func, not a closure: the GraphQL field name is derived
// from the constructor's name (NewGetItem -> getItem).
var maskGQLSeen int

func NewGetItem(_ *Service, p Params[maskGQLArgs]) (*maskItem, error) {
	maskGQLSeen = p.Args.ID
	return &maskItem{ID: p.Args.ID, OwnerID: 7, Count: 3, Title: "hello"}, nil
}

// Output fields become the MaskedID scalar (graphql-go coerces every field
// through its declared type, so a response rewrite can't work). Arguments
// deliberately stay Int: the masked value in `variables` is converted back
// to an integer when the request body is bound, before graphql-go ever
// sees it, which keeps the SDL and the generated client unchanged.
func TestMaskID_GraphQLScalarRoundTrip(t *testing.T) {
	installTestMask(t)
	maskGQLSeen = 0
	mod := Module("maskid_gql",
		Provide(func(app *App) *Service { return app.Service("gqlitems") }),
		AsQuery(NewGetItem),
	)
	app, err := newApp(Config{Server: ServerConfig{Addr: "127.0.0.1:0"}}, mod)
	if err != nil {
		t.Fatalf("newApp: %v", err)
	}
	srv := httptest.NewServer(app.Engine())
	defer func() { srv.Close(); app.Stop() }()

	body := bytes.NewBufferString(`{"query":"query G($id: Int!) { getItem(id: $id) { id ownerId count title } }",` +
		`"variables":{"id":"mask-41"}}`)
	resp, err := http.Post(srv.URL+"/graphql", "application/json", body)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	blob, _ := io.ReadAll(resp.Body)

	var out struct {
		Data   map[string]map[string]any  `json:"data"`
		Errors []struct{ Message string } `json:"errors"`
	}
	if err := json.Unmarshal(blob, &out); err != nil {
		t.Fatalf("decode %s: %v", blob, err)
	}
	if len(out.Errors) > 0 {
		t.Fatalf("graphql errors: %s", blob)
	}
	if maskGQLSeen != 41 {
		t.Errorf("resolver received id %d, want 41", maskGQLSeen)
	}
	item := out.Data["getItem"]
	if item["id"] != "mask-41" || item["ownerId"] != "mask-7" {
		t.Errorf("ids not masked in SDL output: %s", blob)
	}
	if item["count"] != float64(3) || item["title"] != "hello" {
		t.Errorf("non-ID fields rewritten: %s", blob)
	}
}

// Distinct type names per test: graph keeps a process-wide registry keyed
// by GraphQL type name, so reusing maskItem here would hand this schema the
// object the previous test already built.
type maskScopedItem struct {
	ID      int `json:"id"`
	OwnerID int `json:"ownerId"`
}

type maskUnscopedItem struct {
	ID      int `json:"id"`
	OwnerID int `json:"ownerId"`
}

func NewGetScoped(_ *Service, p Params[maskGQLArgs]) (*maskScopedItem, error) {
	return &maskScopedItem{ID: p.Args.ID, OwnerID: 7}, nil
}

func NewGetUnscoped(_ *Service, p Params[maskGQLArgs]) (*maskUnscopedItem, error) {
	return &maskUnscopedItem{ID: p.Args.ID, OwnerID: 7}, nil
}

// The scalar swap happens at schema-build time, so this is where a broken
// scope would be least visible: the SDL itself has to differ per type.
func TestMaskID_GraphQLHonoursTheTypeScope(t *testing.T) {
	maskhook.Install(maskhook.Hooks{
		IsID:        func(key string) bool { return key == "id" || key == "ownerId" },
		Mask:        func(_ string, n int64) (string, bool) { return "mask-" + strconv.FormatInt(n, 10), true },
		Unmask:      func(_, s string) (int64, bool) { return 0, false },
		TypeAllowed: func(name string) bool { return name == "maskScopedItem" },
	})
	t.Cleanup(maskhook.Uninstall)

	mod := Module("maskid_gql_scope",
		Provide(func(app *App) *Service { return app.Service("scopeitems") }),
		AsQuery(NewGetScoped),
		AsQuery(NewGetUnscoped),
	)
	app, err := newApp(Config{Server: ServerConfig{Addr: "127.0.0.1:0"}}, mod)
	if err != nil {
		t.Fatalf("newApp: %v", err)
	}
	srv := httptest.NewServer(app.Engine())
	defer func() { srv.Close(); app.Stop() }()

	query := func(field string) map[string]any {
		t.Helper()
		body := bytes.NewBufferString(`{"query":"query G($id: Int!) { ` + field +
			`(id: $id) { id ownerId } }","variables":{"id":41}}`)
		resp, err := http.Post(srv.URL+"/graphql", "application/json", body)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		blob, _ := io.ReadAll(resp.Body)
		var out struct {
			Data   map[string]map[string]any  `json:"data"`
			Errors []struct{ Message string } `json:"errors"`
		}
		if err := json.Unmarshal(blob, &out); err != nil {
			t.Fatalf("decode %s: %v", blob, err)
		}
		if len(out.Errors) > 0 {
			t.Fatalf("graphql errors on %s: %s", field, blob)
		}
		return out.Data[field]
	}

	in := query("getScoped")
	if in["id"] != "mask-41" || in["ownerId"] != "mask-7" {
		t.Errorf("in-scope type was not masked: %#v", in)
	}
	out := query("getUnscoped")
	if out["id"] != float64(41) || out["ownerId"] != float64(7) {
		t.Errorf("out-of-scope type was masked: %#v", out)
	}
}
