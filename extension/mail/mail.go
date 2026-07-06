// Package mail is nexus's outbound email abstraction — the Go equivalent
// of Laravel's Mail / Rails ActionMailer / Django's email backends.
// Application code composes a Message and hands it to a single Mailer
// interface; the concrete transport (an SMTP server, or a dev log sink) is
// chosen by config, so swapping "print to the console in dev" for "send via
// SMTP in production" is a config change, not a code change.
//
// Two backends ship, both dependency-free:
//
//   - SMTP — any SMTP server, spoken over stdlib net/smtp with STARTTLS,
//     implicit TLS (SMTPS), and PLAIN auth. Builds a proper MIME message
//     (multipart/alternative for text+HTML, multipart/mixed for
//     attachments). No third-party mail library is linked.
//   - Log  — renders the message to an io.Writer (stdout by default) and
//     sends nothing. The safe default for dev/tests: no server, no risk of
//     mailing real users.
//
// Wire a mailer the same way you wire a cache, database, or disk — a typed
// Bind that embeds *mail.Manager, injected into handlers and shown on the
// dashboard:
//
//	type Mailer struct{ *mail.Manager }
//
//	nexus.Run(cfg, mail.Bind[Mailer]("smtp", func() mail.Config {
//	    return mail.Config{
//	        Driver:      "smtp",
//	        Host:        nexus.Get[string]("mail.host"),
//	        Port:        nexus.Get[int]("mail.port", 587),
//	        Username:    nexus.Get[string]("mail.username"),
//	        Password:    nexus.Get[string]("mail.password"),
//	        Encryption:  "starttls",
//	        FromAddress: "no-reply@example.com",
//	        FromName:    "Example",
//	    }
//	}, mail.WithDefault()))
//
// A handler then injects *Mailer and calls Send directly (Manager embeds
// the Mailer):
//
//	func NewSendWelcome(m *Mailer, p nexus.Params[Req]) (*Resp, error) {
//	    err := m.Send(p.Context, mail.Message{
//	        To:      []string{p.Args.Email},
//	        Subject: "Welcome",
//	        Text:    "Thanks for signing up!",
//	        HTML:    "<p>Thanks for signing up!</p>",
//	    })
//	    ...
//	}
package mail

import (
	"context"
	"errors"
	"time"
)

// ErrNoRecipients is returned by Send when a Message has no To/Cc/Bcc
// address — a common mistake worth catching before a transport round-trip.
var ErrNoRecipients = errors.New("mail: message has no recipients")

// Message is a backend-neutral email. From is optional — a Mailer fills in
// its configured default sender when Message.From is empty. Provide Text,
// HTML, or both; a text+HTML message is sent as multipart/alternative so
// clients pick the richest part they render.
type Message struct {
	From    Address           `json:"from"`
	To      []string          `json:"to,omitempty"`
	Cc      []string          `json:"cc,omitempty"`
	Bcc     []string          `json:"bcc,omitempty"`
	ReplyTo string            `json:"replyTo,omitempty"`
	Subject string            `json:"subject,omitempty"`
	Text    string            `json:"text,omitempty"` // plain-text body
	HTML    string            `json:"html,omitempty"` // optional HTML body
	Headers map[string]string `json:"headers,omitempty"`

	Attachments []Attachment `json:"attachments,omitempty"`
}

// Address is an email address with an optional display name. The zero
// value (empty Email) means "unset" — used so Message.From can defer to
// the Mailer's configured sender.
type Address struct {
	Name  string `json:"name,omitempty"`
	Email string `json:"email"`
}

// String renders the address as an RFC 5322 header value:
// `Name <email>` when a name is set, else the bare address.
func (a Address) String() string {
	if a.Email == "" {
		return ""
	}
	if a.Name == "" {
		return a.Email
	}
	return encodeDisplayName(a.Name) + " <" + a.Email + ">"
}

// Attachment is a file to attach. Content is the raw bytes; ContentType
// defaults to application/octet-stream (or is guessed from Filename) when
// empty.
type Attachment struct {
	Filename    string `json:"filename"`
	ContentType string `json:"contentType,omitempty"`
	Content     []byte `json:"-"`
}

// recipients returns every distinct envelope recipient (To+Cc+Bcc), the
// set a transport actually delivers to.
func (m Message) recipients() []string {
	out := make([]string, 0, len(m.To)+len(m.Cc)+len(m.Bcc))
	out = append(out, m.To...)
	out = append(out, m.Cc...)
	out = append(out, m.Bcc...)
	return out
}

// Mailer is the backend-neutral send contract. Send takes a context so a
// slow SMTP dial/handshake honors request cancellation and the configured
// timeout.
type Mailer interface {
	// Send delivers msg. Implementations fill Message.From from their
	// configured default sender when it is unset, and return
	// ErrNoRecipients when the message addresses no one.
	Send(ctx context.Context, msg Message) error
}

// described is the optional interface a Mailer implements to feed the
// dashboard resource. Both built-in mailers satisfy it; a third-party
// Mailer that doesn't just gets generic metadata.
type described interface {
	Driver() string              // "smtp" | "log"
	MailDetails() map[string]any // host/port/encryption/from, for the dashboard
	Ping(context.Context) error  // cheap health probe (nil = healthy)
}

// defaultTimeout is the dial/send deadline applied when Config.Timeout is
// zero. Generous enough for a slow relay, short enough to fail a wedged
// connection before it stalls a request.
const defaultTimeout = 30 * time.Second
