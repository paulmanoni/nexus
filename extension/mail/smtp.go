package mail

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/hex"
	"fmt"
	"net"
	"net/smtp"
	"strconv"
	"time"
)

// Encryption selects the transport security for an SMTP connection.
type Encryption string

const (
	// EncryptionNone sends over a plaintext connection (dev relays only).
	EncryptionNone Encryption = "none"
	// EncryptionStartTLS upgrades a plaintext connection to TLS via the
	// STARTTLS command (the common submission mode, port 587).
	EncryptionStartTLS Encryption = "starttls"
	// EncryptionTLS dials TLS directly (implicit TLS / SMTPS, port 465).
	EncryptionTLS Encryption = "tls"
)

// SMTPMailer sends mail through an SMTP server over stdlib net/smtp. It
// builds a full MIME message and handles STARTTLS, implicit TLS, and PLAIN
// auth. Zero third-party dependencies.
type SMTPMailer struct {
	Host       string
	Port       int
	Username   string
	Password   string
	Encryption Encryption
	From       Address       // default sender when Message.From is empty
	Timeout    time.Duration // dial + send deadline; 0 → defaultTimeout
	// TLSConfig overrides the TLS settings for STARTTLS / implicit TLS.
	// nil uses a config with ServerName set to Host.
	TLSConfig *tls.Config

	// now/boundary are injectable for deterministic tests.
	now      func() time.Time
	boundary func(part string) string
}

// Send builds the MIME message and delivers it. It fills Message.From from
// the configured sender when unset and returns ErrNoRecipients for an
// unaddressed message.
func (s *SMTPMailer) Send(ctx context.Context, msg Message) error {
	rcpts := msg.recipients()
	if len(rcpts) == 0 {
		return ErrNoRecipients
	}
	if err := validateAddresses(msg); err != nil {
		return err
	}

	from := msg.From
	if from.Email == "" {
		from = s.From
	}
	if from.Email == "" {
		return fmt.Errorf("mail: no From address (set Message.From or Config.FromAddress)")
	}

	raw, err := s.builder().build(msg)
	if err != nil {
		return fmt.Errorf("mail: build message: %w", err)
	}

	c, err := s.dial(ctx)
	if err != nil {
		return err
	}
	defer c.Close()

	if s.Username != "" {
		auth := smtp.PlainAuth("", s.Username, s.Password, s.Host)
		if err := c.Auth(auth); err != nil {
			return fmt.Errorf("mail: smtp auth: %w", err)
		}
	}
	if err := c.Mail(from.Email); err != nil {
		return fmt.Errorf("mail: MAIL FROM: %w", err)
	}
	for _, rcpt := range rcpts {
		addr, perr := parseEnvelopeAddr(rcpt)
		if perr != nil {
			return perr
		}
		if err := c.Rcpt(addr); err != nil {
			return fmt.Errorf("mail: RCPT TO %s: %w", addr, err)
		}
	}
	w, err := c.Data()
	if err != nil {
		return fmt.Errorf("mail: DATA: %w", err)
	}
	if _, err := w.Write(raw); err != nil {
		return fmt.Errorf("mail: write body: %w", err)
	}
	if err := w.Close(); err != nil {
		return fmt.Errorf("mail: close body: %w", err)
	}
	return c.Quit()
}

// dial establishes an *smtp.Client honoring the encryption mode, context,
// and timeout.
func (s *SMTPMailer) dial(ctx context.Context) (*smtp.Client, error) {
	addr := net.JoinHostPort(s.Host, strconv.Itoa(s.Port))
	timeout := s.Timeout
	if timeout <= 0 {
		timeout = defaultTimeout
	}
	dialer := &net.Dialer{Timeout: timeout}

	var conn net.Conn
	var err error
	if s.Encryption == EncryptionTLS {
		conn, err = tls.DialWithDialer(dialer, "tcp", addr, s.tlsConfig())
	} else {
		conn, err = dialer.DialContext(ctx, "tcp", addr)
	}
	if err != nil {
		return nil, fmt.Errorf("mail: dial %s: %w", addr, err)
	}
	// Bound the whole SMTP conversation by the same deadline.
	if dl, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(dl)
	} else {
		_ = conn.SetDeadline(s.clock().Add(timeout))
	}

	c, err := smtp.NewClient(conn, s.Host)
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("mail: smtp handshake: %w", err)
	}

	if s.Encryption == EncryptionStartTLS {
		if ok, _ := c.Extension("STARTTLS"); !ok {
			c.Close()
			return nil, fmt.Errorf("mail: server %s does not support STARTTLS", addr)
		}
		if err := c.StartTLS(s.tlsConfig()); err != nil {
			c.Close()
			return nil, fmt.Errorf("mail: STARTTLS: %w", err)
		}
	}
	return c, nil
}

func (s *SMTPMailer) tlsConfig() *tls.Config {
	if s.TLSConfig != nil {
		return s.TLSConfig
	}
	return &tls.Config{ServerName: s.Host, MinVersion: tls.VersionTLS12}
}

func (s *SMTPMailer) clock() time.Time {
	if s.now != nil {
		return s.now()
	}
	return time.Now()
}

// builder assembles a mimeBuilder with this mailer's sender/clock/boundary.
func (s *SMTPMailer) builder() mimeBuilder {
	now := s.now
	if now == nil {
		now = nowFunc
	}
	boundary := s.boundary
	if boundary == nil {
		boundary = randomBoundary
	}
	return mimeBuilder{from: s.From, now: now, boundary: boundary}
}

// --- described (dashboard resource) ---

func (s *SMTPMailer) Driver() string { return "smtp" }
func (s *SMTPMailer) MailDetails() map[string]any {
	enc := string(s.Encryption)
	if enc == "" {
		enc = "none"
	}
	return map[string]any{
		"driver":     "smtp",
		"host":       s.Host,
		"port":       s.Port,
		"encryption": enc,
		"from":       s.From.Email,
		"auth":       s.Username != "",
	}
}

// Ping opens (and immediately closes) an SMTP connection to verify the
// server is reachable and the TLS/greeting handshake succeeds. It does not
// authenticate or send, so it's cheap enough for dashboard health polls.
func (s *SMTPMailer) Ping(ctx context.Context) error {
	c, err := s.dial(ctx)
	if err != nil {
		return err
	}
	defer c.Close()
	return c.Noop()
}

// randomBoundary returns a unique multipart boundary. The part label keeps
// nested boundaries (mixed vs alt) distinct and readable.
func randomBoundary(part string) string {
	var b [12]byte
	_, _ = rand.Read(b[:])
	return "nexus-" + part + "-" + hex.EncodeToString(b[:])
}

// parseEnvelopeAddr extracts the bare address from a possibly
// display-named recipient ("Bob <bob@x.com>" → "bob@x.com") for the RCPT
// TO command.
func parseEnvelopeAddr(s string) (string, error) {
	a, err := parseAddress(s)
	if err != nil {
		return "", fmt.Errorf("mail: invalid recipient %q: %w", s, err)
	}
	return a, nil
}
