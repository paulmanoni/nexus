package template_test

import (
	"testing"
	"time"

	"github.com/paulmanoni/nexus/live"
	"github.com/paulmanoni/nexus/live/template"
)

// External-package test ensures the test harness compiles and
// works from outside the template package — i.e., that we're
// not accidentally leaning on unexported symbols.

type counter struct {
	template.BaseComponent
	Count int
}

func (c *counter) Inc(_ *template.Ctx)                      { c.Count++ }
func (c *counter) AddBy(_ *template.Ctx, p template.Payload) { c.Count += p.Int("by") }

const counterSrc = `<template><span id="count">{{ Count }}</span></template>`

func TestRenderOnce_ProducesInitialHTML(t *testing.T) {
	e := template.New(template.WithNotifier(live.New()))
	if err := e.Register("Counter", []byte(counterSrc), func() template.Component {
		return &counter{}
	}); err != nil {
		t.Fatalf("register: %v", err)
	}
	got, err := e.RenderOnce("Counter", nil)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	want := `<span id="count">0</span>`
	if got != want {
		t.Errorf("got %q want %q", got, want)
	}
}

func TestNewTestSession_EventRoundTrip(t *testing.T) {
	e := template.New(template.WithNotifier(live.New()))
	if err := e.Register("Counter", []byte(counterSrc), func() template.Component {
		return &counter{}
	}); err != nil {
		t.Fatalf("register: %v", err)
	}

	tr, stop, err := e.NewTestSession("Counter", nil)
	if err != nil {
		t.Fatalf("session: %v", err)
	}
	defer stop()

	// First frame should be "joined" with the initial Rendered.
	joined, ok := tr.NextOut(2 * time.Second)
	if !ok {
		t.Fatal("no joined frame")
	}
	if joined.Type != "joined" {
		t.Fatalf("first frame type = %q want joined; payload=%+v", joined.Type, joined)
	}

	// Fire an event; expect a diff frame with the new count.
	tr.Push(template.Inbound{Type: "event", Name: "inc"})
	diff, ok := tr.NextOut(2 * time.Second)
	if !ok {
		t.Fatal("no diff after inc")
	}
	if diff.Type != "diff" {
		t.Errorf("got %+v want diff", diff)
	}
}
