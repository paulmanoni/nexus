package nexus

import (
	"bytes"
	"context"
	"io"
	"mime/multipart"
	"net/http/httptest"
	"strings"
	"testing"
)

// nexus.Form: source-unified raw reads (JSON / multipart / urlencoded /
// query), file streaming, Bind back into the typed world, FormFrom on the
// handler's ctx — and nexus.Errors rendering as 422 on REST.

type formEcho struct {
	Title string   `json:"title"`
	Tags  []string `json:"tags"`
	Page  int      `json:"page"`
	Draft bool     `json:"draft"`
	File  string   `json:"file"`
	Sub   string   `json:"sub"`
}

func formApp(t *testing.T) (*App, func()) {
	t.Helper()
	app, stop, err := InProcess(Config{},
		AsRest("POST", "/echo", func(ctx context.Context, fm *Form) (*formEcho, error) {
			out := &formEcho{
				Title: fm.Get("title"),
				Tags:  fm.All("tags"),
				Page:  fm.Int("page", 1),
				Draft: fm.Bool("draft", false),
			}
			if sub := FormFrom(ctx); sub != nil {
				out.Sub = sub.Get("title")
			}
			if ff, err := fm.File("doc"); err == nil {
				r, err := ff.Open()
				if err != nil {
					return nil, err
				}
				defer r.Close()
				b, _ := io.ReadAll(r)
				out.File = ff.Name() + ":" + string(b)
			}
			return out, nil
		}),
		AsRest("POST", "/bind", func(fm *Form) (*formEcho, error) {
			if fm.Get("title") == "" { // raw read first, then Bind must still work
				return nil, NewErrors().Field("title", "required")
			}
			var dto formEcho
			if err := fm.Bind(&dto); err != nil {
				return nil, err
			}
			return &dto, nil
		}),
	)
	if err != nil {
		t.Fatal(err)
	}
	return app, func() { _ = stop(context.Background()) }
}

func TestFormJSONBody(t *testing.T) {
	app, done := formApp(t)
	defer done()

	body := `{"title":"hello","tags":["a","b"],"page":3,"draft":true}`
	w := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/echo", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	app.ServeHTTP(w, req)
	got := w.Body.String()
	for _, want := range []string{`"title":"hello"`, `"tags":["a","b"]`, `"page":3`, `"draft":true`, `"sub":"hello"`} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %s in %s", want, got)
		}
	}
}

func TestFormMultipartWithFile(t *testing.T) {
	app, done := formApp(t)
	defer done()

	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	_ = mw.WriteField("title", "upload")
	_ = mw.WriteField("tags", "x")
	_ = mw.WriteField("tags", "y")
	fw, _ := mw.CreateFormFile("doc", "cv.txt")
	_, _ = fw.Write([]byte("PDFBYTES"))
	_ = mw.Close()

	w := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/echo?page=7", &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	app.ServeHTTP(w, req)
	got := w.Body.String()
	for _, want := range []string{`"title":"upload"`, `"tags":["x","y"]`, `"page":7`, `"file":"cv.txt:PDFBYTES"`} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %s in %s", want, got)
		}
	}
}

func TestFormURLEncodedAndQueryFallback(t *testing.T) {
	app, done := formApp(t)
	defer done()

	w := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/echo?title=fromquery&page=9",
		strings.NewReader("title=frombody&draft=on"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	app.ServeHTTP(w, req)
	got := w.Body.String()
	// Body wins over query for title; query fills page the body lacks.
	if !strings.Contains(got, `"title":"frombody"`) || !strings.Contains(got, `"page":9`) || !strings.Contains(got, `"draft":true`) {
		t.Fatalf("got %s", got)
	}
}

func TestFormBindAfterRawRead(t *testing.T) {
	app, done := formApp(t)
	defer done()

	w := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/bind", strings.NewReader(`{"title":"typed","page":2}`))
	req.Header.Set("Content-Type", "application/json")
	app.ServeHTTP(w, req)
	if !strings.Contains(w.Body.String(), `"title":"typed"`) || !strings.Contains(w.Body.String(), `"page":2`) {
		t.Fatalf("bind after raw read: %s", w.Body.String())
	}
}

func TestErrorsRenderAs422(t *testing.T) {
	app, done := formApp(t)
	defer done()

	w := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/bind", strings.NewReader(`{}`))
	req.Header.Set("Content-Type", "application/json")
	app.ServeHTTP(w, req)
	if w.Code != 422 {
		t.Fatalf("status = %d body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"title":["required"]`) {
		t.Fatalf("422 body = %s", w.Body.String())
	}
}

func TestErrorsAccumulator(t *testing.T) {
	e := NewErrors()
	if e.Any() {
		t.Fatal("fresh accumulator must be empty")
	}
	e.Field("email", "taken").Field("email", "invalid").Global("provider down")
	if !e.Any() {
		t.Fatal("Any after adds")
	}
	fe := e.FieldErrors()
	if len(fe["email"]) != 2 || fe[GlobalErrorKey][0] != "provider down" {
		t.Fatalf("FieldErrors = %v", fe)
	}
	first := e.First()
	if first["email"] != "taken" || first[GlobalErrorKey] != "provider down" {
		t.Fatalf("First = %v", first)
	}
	ext := e.Extensions()
	if ext["code"] != "VALIDATION" {
		t.Fatalf("Extensions = %v", ext)
	}
}

// A *Form param on a non-REST transport must be a typed nil whose methods
// no-op rather than panic.
func TestFormNilOnGraphQL(t *testing.T) {
	app, stop, err := InProcess(Config{},
		AsQuery(func(ctx context.Context, fm *Form) (*formEcho, error) {
			if fm != nil {
				return nil, NewErrors().Global("form must be nil off REST")
			}
			_ = fm.Get("x") // nil receiver must not panic
			return &formEcho{Title: "gql-ok"}, nil
		}, Op("formProbe")),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = stop(context.Background()) }()
	if got := postGraphQL(t, app, `{ formProbe { title } }`); !strings.Contains(got, "gql-ok") {
		t.Fatalf("gql = %s", got)
	}
}
