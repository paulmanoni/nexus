package view

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/paulmanoni/nexus"
)

func TestAssets(t *testing.T) {
	lib := http.NewServeMux()
	lib.HandleFunc("GET /lib/js/{file}", func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "script "+r.PathValue("file"))
	})
	app, stop, err := nexus.InProcess(nexus.Config{}, Assets("/lib/js/", lib))
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
