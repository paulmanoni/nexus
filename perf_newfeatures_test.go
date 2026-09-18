package nexus

import (
	"context"
	"net/http/httptest"
	"testing"
)

// Benchmarks for the v1.51–v1.54 handler features: what an op pays for
// Arg / Envelope relative to a hand-written wrapper, and what EVERY op
// pays for the Form slot machinery.

type perfSvc struct{}

type perfIDArgs struct {
	ID uint `json:"id" query:"id"`
}

type perfOut struct {
	V uint `json:"v"`
}

func (s *perfSvc) Fetch(id uint) (*perfOut, error) { return &perfOut{V: id}, nil }

func perfWrapped(svc *perfSvc, a perfIDArgs) (*perfOut, error) { return svc.Fetch(a.ID) }

func perfEnv[T any](v T, err error) (*envResp[T], error) {
	if err != nil {
		return &envResp[T]{Status: false, Message: err.Error()}, nil
	}
	return &envResp[T]{Status: true, Data: v}, nil
}

func benchApp(b *testing.B, opts ...Option) *App {
	b.Helper()
	app, stop, err := InProcess(Config{}, append([]Option{Supply(&perfSvc{})}, opts...)...)
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { _ = stop(context.Background()) })
	return app
}

func benchGet(b *testing.B, app *App, path string) {
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		w := httptest.NewRecorder()
		app.ServeHTTP(w, httptest.NewRequest("GET", path, nil))
		if w.Code != 200 {
			b.Fatalf("status %d: %s", w.Code, w.Body.String())
		}
	}
}

// Baseline: the hand-written wrapper struct adapter.
func BenchmarkRestWrappedBaseline(b *testing.B) {
	app := benchApp(b, AsRest("GET", "/f", perfWrapped))
	benchGet(b, app, "/f?id=7")
}

// Same op through nexus.Arg (MakeFunc trampoline).
func BenchmarkRestArg(b *testing.B) {
	app := benchApp(b, AsRest("GET", "/f", (*perfSvc).Fetch, Arg("id")))
	benchGet(b, app, "/f?id=7")
}

// Baseline + Envelope.
func BenchmarkRestEnvelope(b *testing.B) {
	app := benchApp(b, AsRest("GET", "/f", perfWrapped, Envelope(perfEnv[*perfOut])))
	benchGet(b, app, "/f?id=7")
}

// A plain no-Form handler: measures the per-request cost of the Form slot
// machinery every op pays.
func BenchmarkRestNoFormScan(b *testing.B) {
	app := benchApp(b, AsRest("GET", "/p", func(svc *perfSvc, a perfIDArgs) (*perfOut, error) {
		return &perfOut{V: a.ID}, nil
	}))
	benchGet(b, app, "/p?id=7")
}
