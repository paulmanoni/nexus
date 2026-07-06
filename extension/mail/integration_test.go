package mail

import (
	"context"
	"strings"
	"testing"

	"github.com/paulmanoni/nexus"
)

// AppMailer is the user-style typed handle for the e2e Bind test.
type AppMailer struct{ *Manager }

// TestBindEndToEnd wires a log mailer through a real nexus app and proves
// the injected handle sends and registers a dashboard resource.
func TestBindEndToEnd(t *testing.T) {
	var got *AppMailer
	app, stop, err := nexus.InProcess(nexus.Config{},
		Bind[AppMailer]("smtp", func() Config {
			return Config{Driver: "log", FromAddress: "no-reply@x.com", FromName: "X"}
		}, WithDefault(), WithDescription("test mailer")),
		nexus.Invoke(func(m *AppMailer) { got = m }),
	)
	if err != nil {
		t.Fatalf("InProcess: %v", err)
	}
	defer stop(context.Background())

	if got == nil {
		t.Fatal("AppMailer was not injected")
	}

	// The injected handle sends (log backend records it).
	var buf strings.Builder
	got.Mailer().(*LogMailer).Writer = &buf
	if err := got.Send(context.Background(), Message{
		To:      []string{"user@x.com"},
		Subject: "Welcome",
		Text:    "hi",
	}); err != nil {
		t.Fatalf("Send via injected handle: %v", err)
	}
	if !strings.Contains(buf.String(), "Welcome") {
		t.Errorf("message not sent through injected mailer:\n%s", buf.String())
	}

	// The mailer should be registered as a dashboard resource.
	found := false
	for _, r := range app.Registry().Resources() {
		if r.Name == "smtp" && string(r.Kind) == "mail" {
			found = true
		}
	}
	if !found {
		t.Error("mail resource was not registered")
	}
}
