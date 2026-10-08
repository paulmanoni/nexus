package view

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/paulmanoni/nexus/v2"
	"github.com/paulmanoni/nexus/v2/config"
)

func TestAssets(t *testing.T) {
	lib := http.NewServeMux()
	lib.HandleFunc("GET /lib/js/{file}", func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "script "+r.PathValue("file"))
	})
	app, stop, err := nexus.InProcess(config.Runtime{}, Assets("/lib/js/", lib))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = stop(context.Background()) }()
	rec := httptest.NewRecorder()
	app.ServeHTTP(rec, httptest.NewRequest("GET", "/lib/js/dialog.min.js", nil))
	if rec.Code != 200 || rec.Body.String() != "script dialog.min.js" {
		t.Fatalf("GET = %d %q", rec.Code, rec.Body.String())
	}
}

// A boot scoped to some annotated modules still serves the runtime its
// pages load.
func TestRuntimeServedUnderDecoratedModules(t *testing.T) {
	app, stop, err := nexus.InProcess(config.Runtime{}, nexus.DecoratedModules())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = stop(context.Background()) }()
	rec := httptest.NewRecorder()
	app.ServeHTTP(rec, httptest.NewRequest("GET", "/_view/runtime.js", nil))
	if rec.Code != 200 || rec.Body.Len() == 0 {
		t.Fatalf("GET /_view/runtime.js = %d", rec.Code)
	}
}
