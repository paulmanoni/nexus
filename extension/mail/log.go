package mail

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"
)

// LogMailer is the dev/test transport: it renders each message and writes
// it to an io.Writer (stdout by default) instead of sending it. It's the
// safe default — no server to configure, no chance of mailing real users
// from a dev box. It also records the last-sent messages so tests can
// assert on them without a fake SMTP server.
type LogMailer struct {
	// Writer receives the rendered message. Defaults to os.Stdout.
	Writer io.Writer
	// From is the default sender applied when Message.From is empty.
	From Address

	mu   sync.Mutex
	sent []Message // capped ring of recent messages, for Sent()
}

// Send renders msg and writes a compact, human-readable summary to the
// configured writer, then records it. It never touches the network.
func (l *LogMailer) Send(ctx context.Context, msg Message) error {
	if len(msg.recipients()) == 0 {
		return ErrNoRecipients
	}
	if err := validateAddresses(msg); err != nil {
		return err
	}

	from := msg.From
	if from.Email == "" {
		from = l.From
	}

	var b strings.Builder
	fmt.Fprintf(&b, "mail(log): would send email\n")
	fmt.Fprintf(&b, "  From:    %s\n", from.String())
	fmt.Fprintf(&b, "  To:      %s\n", strings.Join(msg.To, ", "))
	if len(msg.Cc) > 0 {
		fmt.Fprintf(&b, "  Cc:      %s\n", strings.Join(msg.Cc, ", "))
	}
	if len(msg.Bcc) > 0 {
		fmt.Fprintf(&b, "  Bcc:     %s\n", strings.Join(msg.Bcc, ", "))
	}
	fmt.Fprintf(&b, "  Subject: %s\n", msg.Subject)
	if msg.Text != "" {
		fmt.Fprintf(&b, "  Text:    %s\n", oneLine(msg.Text, 200))
	}
	if msg.HTML != "" {
		fmt.Fprintf(&b, "  HTML:    %d bytes\n", len(msg.HTML))
	}
	for _, att := range msg.Attachments {
		fmt.Fprintf(&b, "  Attach:  %s (%d bytes)\n", att.Filename, len(att.Content))
	}

	w := l.Writer
	if w == nil {
		w = os.Stdout
	}
	if _, err := io.WriteString(w, b.String()); err != nil {
		return err
	}

	l.record(msg)
	return nil
}

// Sent returns a copy of the messages this mailer has recorded, oldest
// first — for assertions in tests.
func (l *LogMailer) Sent() []Message {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]Message, len(l.sent))
	copy(out, l.sent)
	return out
}

// record appends to the capped ring so a long-running dev server doesn't
// grow this unbounded.
func (l *LogMailer) record(msg Message) {
	const max = 100
	l.mu.Lock()
	defer l.mu.Unlock()
	l.sent = append(l.sent, msg)
	if len(l.sent) > max {
		l.sent = l.sent[len(l.sent)-max:]
	}
}

// --- described (dashboard resource) ---

func (l *LogMailer) Driver() string { return "log" }
func (l *LogMailer) MailDetails() map[string]any {
	from := l.From.Email
	if from == "" {
		from = "(unset)"
	}
	return map[string]any{"driver": "log", "from": from, "note": "writes to console; sends nothing"}
}

// Ping is always healthy — there's no server to reach.
func (l *LogMailer) Ping(context.Context) error { return nil }

// oneLine collapses whitespace and truncates s for a compact log line.
func oneLine(s string, max int) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > max {
		return s[:max] + "…"
	}
	return s
}

// compile-time timestamp helper reused by builders that need a clock; kept
// here so both mailers share one default.
func nowFunc() time.Time { return time.Now() }
