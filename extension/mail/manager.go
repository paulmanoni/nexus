package mail

import (
	"context"
	"time"

	"github.com/paulmanoni/nexus/resource"
)

// Manager is the injectable mail handle. User code embeds *Manager in a
// named type (type Mailer struct{ *mail.Manager }) so the DI container
// routes by type; the Manager delegates Send to the underlying transport,
// so a handler calls mailer.Send(...) directly. It also adapts the
// transport into a dashboard resource.
type Manager struct {
	mailer Mailer
}

// NewManager wraps a Mailer. Rarely called directly — Bind builds one from
// Config — but exported so tests and advanced wiring can construct a
// Manager around a custom Mailer (e.g. a fake in tests).
func NewManager(m Mailer) *Manager { return &Manager{mailer: m} }

// Mailer returns the underlying transport for callers that need the raw
// interface.
func (m *Manager) Mailer() Mailer { return m.mailer }

// Send delivers msg through the configured transport. A Manager IS a
// Mailer for day-to-day use.
func (m *Manager) Send(ctx context.Context, msg Message) error {
	return m.mailer.Send(ctx, msg)
}

// AsResource adapts the transport into a dashboard resource. Health is a
// cheap probe (log: no-op; SMTP: dial + NOOP) so snapshotting stays fast;
// real send failures surface on the operation itself.
func (m *Manager) AsResource(name, desc string, opts ...resource.Option) resource.Resource {
	driver := "mail"
	details := map[string]any{}
	if d, ok := m.mailer.(described); ok {
		driver = d.Driver()
		details = d.MailDetails()
	}
	if desc == "" {
		desc = "outbound mail (" + driver + ")"
	}
	healthy := func() bool {
		d, ok := m.mailer.(described)
		if !ok {
			return true
		}
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		return d.Ping(ctx) == nil
	}
	return resource.NewMail(name, desc, details, healthy, opts...)
}
