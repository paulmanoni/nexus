package mail

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestLogMailer_SendWritesAndRecords(t *testing.T) {
	var buf strings.Builder
	m := &LogMailer{Writer: &buf, From: Address{Name: "App", Email: "app@x.com"}}

	err := m.Send(context.Background(), Message{
		To:          []string{"bob@x.com"},
		Cc:          []string{"cc@x.com"},
		Subject:     "Hi",
		Text:        "line one\nline two",
		Attachments: []Attachment{{Filename: "a.txt", Content: []byte("data")}},
	})
	if err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	for _, want := range []string{
		"App <app@x.com>", "bob@x.com", "cc@x.com", "Subject: Hi",
		"line one line two", "a.txt (4 bytes)",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("log output missing %q\n%s", want, out)
		}
	}
	if got := m.Sent(); len(got) != 1 || got[0].Subject != "Hi" {
		t.Errorf("Sent() = %+v, want 1 recorded message", got)
	}
}

func TestLogMailer_NoRecipients(t *testing.T) {
	m := &LogMailer{Writer: &strings.Builder{}}
	if err := m.Send(context.Background(), Message{Subject: "x"}); !errors.Is(err, ErrNoRecipients) {
		t.Errorf("want ErrNoRecipients, got %v", err)
	}
}

func TestLogMailer_InvalidRecipient(t *testing.T) {
	m := &LogMailer{Writer: &strings.Builder{}}
	err := m.Send(context.Background(), Message{To: []string{"nope"}})
	if err == nil || errors.Is(err, ErrNoRecipients) {
		t.Errorf("want an address-validation error, got %v", err)
	}
}

func TestLogMailer_Described(t *testing.T) {
	m := &LogMailer{From: Address{Email: "a@b.com"}}
	if m.Driver() != "log" {
		t.Errorf("Driver() = %q", m.Driver())
	}
	if m.Ping(context.Background()) != nil {
		t.Error("log Ping should always be healthy")
	}
	if d := m.MailDetails(); d["from"] != "a@b.com" {
		t.Errorf("MailDetails from = %v", d["from"])
	}
}

func TestLogMailer_RecordRingCap(t *testing.T) {
	m := &LogMailer{Writer: &strings.Builder{}}
	for range 150 {
		_ = m.Send(context.Background(), Message{To: []string{"a@b.com"}, Subject: "s"})
	}
	if got := len(m.Sent()); got != 100 {
		t.Errorf("Sent() len = %d, want capped at 100", got)
	}
}
