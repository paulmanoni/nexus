package nexus

import (
	"context"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"
)

// nexus.Envelope: handlers return plain (T, error); the app-supplied wrap
// owns the wire shape, including turning errors into 200/data payloads.

type envResp[T any] struct {
	Status  bool   `json:"status"`
	Message string `json:"message"`
	Data    T      `json:"data"`
}

func envWrap[T any](v T, err error) (*envResp[T], error) {
	if err != nil {
		return &envResp[T]{Status: false, Message: err.Error()}, nil
	}
	return &envResp[T]{Status: true, Message: "ok", Data: v}, nil
}

type envSvc struct{}

func (s *envSvc) FindUser(ctx context.Context, a probeArgs) (*probeOut, error) {
	if a.Name == "missing" {
		return nil, errors.New("no such user")
	}
	return &probeOut{Greeting: "found " + a.Name}, nil
}

func TestEnvelopeGraphQL(t *testing.T) {
	app, stop, err := InProcess(Config{},
		Supply(&envSvc{}),
		AsQuery((*envSvc).FindUser, Envelope(envWrap[*probeOut])),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = stop(context.Background()) }()

	got := postGraphQL(t, app, `{ findUser(name: "ada") { status message data { greeting } } }`)
	if !strings.Contains(got, `"status":true`) || !strings.Contains(got, "found ada") {
		t.Fatalf("success = %s", got)
	}

	got = postGraphQL(t, app, `{ findUser(name: "missing") { status message data { greeting } } }`)
	if !strings.Contains(got, `"status":false`) || !strings.Contains(got, "no such user") {
		t.Fatalf("error-in-envelope = %s", got)
	}
	if strings.Contains(got, `"errors"`) {
		t.Fatalf("enveloped error must not reach the GraphQL errors array: %s", got)
	}
}

func TestEnvelopeRest(t *testing.T) {
	app, stop, err := InProcess(Config{},
		Supply(&envSvc{}),
		AsRest("GET", "/find", (*envSvc).FindUser, Envelope(envWrap[*probeOut])),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = stop(context.Background()) }()

	w := httptest.NewRecorder()
	app.ServeHTTP(w, httptest.NewRequest("GET", "/find?name=bo", nil))
	if w.Code != 200 || !strings.Contains(w.Body.String(), "found bo") {
		t.Fatalf("success = %d %s", w.Code, w.Body.String())
	}

	w = httptest.NewRecorder()
	app.ServeHTTP(w, httptest.NewRequest("GET", "/find?name=missing", nil))
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"status":false`) || !strings.Contains(w.Body.String(), "no such user") {
		t.Fatalf("enveloped error must be a 200 payload: %d %s", w.Code, w.Body.String())
	}
}

// A wrap whose input type doesn't match the handler's return must fail at
// boot, naming both types.
func TestEnvelopeTypeMismatchFailsBoot(t *testing.T) {
	_, stop, err := InProcess(Config{},
		Supply(&envSvc{}),
		AsQuery((*envSvc).FindUser, Envelope(envWrap[[]string])),
	)
	if stop != nil {
		defer func() { _ = stop(context.Background()) }()
	}
	if err == nil || !strings.Contains(err.Error(), "Envelope wrap takes") {
		t.Fatalf("expected boot failure naming the mismatch, got %v", err)
	}
}
