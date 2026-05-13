package client

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

func init() {
	gin.SetMode(gin.TestMode)
}

// TestContributionsHandler_HappyPath serves a stub builder and
// asserts the JSON shape matches the ContributionsResponse contract.
// The Version field is filled in by the handler when the builder
// leaves it blank, so callers don't have to thread SchemaVersion
// through every render.
func TestContributionsHandler_HappyPath(t *testing.T) {
	build := func(framework string) (ContributionsResponse, error) {
		return ContributionsResponse{
			Plugins: []ContributionPluginRec{
				{
					Name: "auth",
					Files: []ContributionFileRec{
						{Path: "auth/vue.ts", Body: "// stub"},
					},
				},
			},
		}, nil
	}
	r := gin.New()
	r.GET("/contributions.json", contributionsHandler(build))

	srv := httptest.NewServer(r)
	defer srv.Close()

	res, err := http.Get(srv.URL + "/contributions.json?framework=vue")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", res.StatusCode)
	}
	var got ContributionsResponse
	if err := json.NewDecoder(res.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Version != SchemaVersion {
		t.Errorf("Version = %q, want %q (handler should fill default)", got.Version, SchemaVersion)
	}
	if got.Framework != "vue" {
		t.Errorf("Framework = %q, want vue (handler should echo the query param)", got.Framework)
	}
	if len(got.Plugins) != 1 || got.Plugins[0].Name != "auth" {
		t.Fatalf("plugins = %+v, want one auth plugin", got.Plugins)
	}
	if got.Plugins[0].Files[0].Body != "// stub" {
		t.Errorf("body = %q, want '// stub'", got.Plugins[0].Files[0].Body)
	}
}

// TestContributionsHandler_FrameworkPassedToBuilder confirms the
// query param reaches the builder so the per-framework branching
// in auth's contributor (or any other) gets the right input.
func TestContributionsHandler_FrameworkPassedToBuilder(t *testing.T) {
	var got string
	build := func(framework string) (ContributionsResponse, error) {
		got = framework
		return ContributionsResponse{}, nil
	}
	r := gin.New()
	r.GET("/contributions.json", contributionsHandler(build))
	srv := httptest.NewServer(r)
	defer srv.Close()

	for _, want := range []string{"vue", "react", "none", ""} {
		_, err := http.Get(srv.URL + "/contributions.json?framework=" + want)
		if err != nil {
			t.Fatalf("GET framework=%q: %v", want, err)
		}
		if got != want {
			t.Errorf("builder saw framework=%q, want %q", got, want)
		}
	}
}

// TestContributionsHandler_BuilderErrorReturns500 makes sure a
// misbehaving contributor surfaces as a 500 with the error in the
// body. The CLI parses this to attribute build failures to the
// right plugin.
func TestContributionsHandler_BuilderErrorReturns500(t *testing.T) {
	build := func(string) (ContributionsResponse, error) {
		return ContributionsResponse{}, errors.New("auth contributor exploded")
	}
	r := gin.New()
	r.GET("/contributions.json", contributionsHandler(build))
	srv := httptest.NewServer(r)
	defer srv.Close()

	res, err := http.Get(srv.URL + "/contributions.json")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", res.StatusCode)
	}
	var body map[string]string
	if err := json.NewDecoder(res.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(body["error"], "auth contributor exploded") {
		t.Errorf("error body = %q, want contributor message", body["error"])
	}
}

// TestContributionsHandler_RespectsBuilderVersion ensures the
// builder can override the default SchemaVersion — useful for
// forward-compatibility experiments where a custom builder wants
// to mark a different wire format.
func TestContributionsHandler_RespectsBuilderVersion(t *testing.T) {
	build := func(string) (ContributionsResponse, error) {
		return ContributionsResponse{Version: "test.v9"}, nil
	}
	r := gin.New()
	r.GET("/contributions.json", contributionsHandler(build))
	srv := httptest.NewServer(r)
	defer srv.Close()

	res, _ := http.Get(srv.URL + "/contributions.json")
	defer res.Body.Close()
	var got ContributionsResponse
	if err := json.NewDecoder(res.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if got.Version != "test.v9" {
		t.Errorf("Version = %q, want builder override 'test.v9'", got.Version)
	}
}
