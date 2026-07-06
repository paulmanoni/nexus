package mail

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	"net/mail"
	"path/filepath"
	"strings"
	"time"
)

// mimeBuilder renders a Message into RFC 5322 / MIME bytes. now and
// boundary are injectable so tests get deterministic output; production
// uses time.Now and a random boundary.
type mimeBuilder struct {
	from     Address                  // resolved default sender
	now      func() time.Time         // Date header source
	boundary func(part string) string // multipart boundary generator
}

// build renders msg to a byte slice ready to hand to a transport's DATA
// command. The envelope sender/recipients are handled separately by the
// transport; this is purely the message content.
func (b mimeBuilder) build(msg Message) ([]byte, error) {
	from := msg.From
	if from.Email == "" {
		from = b.from
	}

	var buf bytes.Buffer
	// --- headers ---
	h := headerWriter{&buf}
	h.set("From", from.String())
	h.set("To", strings.Join(msg.To, ", "))
	if len(msg.Cc) > 0 {
		h.set("Cc", strings.Join(msg.Cc, ", "))
	}
	if msg.ReplyTo != "" {
		h.set("Reply-To", msg.ReplyTo)
	}
	h.set("Subject", mime.QEncoding.Encode("utf-8", msg.Subject))
	h.set("Date", b.now().Format(time.RFC1123Z))
	h.set("MIME-Version", "1.0")
	// Custom headers last so callers can override nothing structural but
	// add things like X-Entity-Ref-ID. Skip ones we own to avoid dupes.
	for k, v := range msg.Headers {
		if isReservedHeader(k) {
			continue
		}
		h.set(k, v)
	}

	// --- body ---
	if len(msg.Attachments) > 0 {
		return b.buildMixed(&buf, h, msg)
	}
	if msg.Text != "" && msg.HTML != "" {
		return b.buildAlternative(&buf, h, msg.Text, msg.HTML)
	}
	// Single part.
	ct := "text/plain; charset=utf-8"
	body := msg.Text
	if msg.HTML != "" {
		ct = "text/html; charset=utf-8"
		body = msg.HTML
	}
	h.set("Content-Type", ct)
	h.set("Content-Transfer-Encoding", "quoted-printable")
	buf.WriteString("\r\n")
	if err := writeQP(&buf, body); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// writeAltParts writes the text and HTML parts of a multipart/alternative
// body (text first, per RFC — least-rich to most-rich).
func (b mimeBuilder) writeAltParts(w io.Writer, boundary, text, html string) error {
	mw := multipart.NewWriter(w)
	if err := mw.SetBoundary(boundary); err != nil {
		return err
	}
	for _, p := range []struct{ ct, body string }{
		{"text/plain; charset=utf-8", text},
		{"text/html; charset=utf-8", html},
	} {
		part, err := mw.CreatePart(map[string][]string{
			"Content-Type":              {p.ct},
			"Content-Transfer-Encoding": {"quoted-printable"},
		})
		if err != nil {
			return err
		}
		if err := writeQP(part, p.body); err != nil {
			return err
		}
	}
	return mw.Close()
}

// buildAlternative renders a text+HTML message as multipart/alternative.
func (b mimeBuilder) buildAlternative(buf *bytes.Buffer, h headerWriter, text, html string) ([]byte, error) {
	boundary := b.boundary("alt")
	h.set("Content-Type", "multipart/alternative; boundary="+boundary)
	buf.WriteString("\r\n")
	if err := b.writeAltParts(buf, boundary, text, html); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// buildMixed renders a message with attachments as multipart/mixed. When
// both text and HTML are present the body is a nested
// multipart/alternative; otherwise it's a single body part.
func (b mimeBuilder) buildMixed(buf *bytes.Buffer, h headerWriter, msg Message) ([]byte, error) {
	mixed := b.boundary("mixed")
	h.set("Content-Type", "multipart/mixed; boundary="+mixed)
	buf.WriteString("\r\n")

	mw := multipart.NewWriter(buf)
	if err := mw.SetBoundary(mixed); err != nil {
		return nil, err
	}

	// Body part.
	if msg.Text != "" && msg.HTML != "" {
		altBoundary := b.boundary("alt")
		part, err := mw.CreatePart(map[string][]string{
			"Content-Type": {"multipart/alternative; boundary=" + altBoundary},
		})
		if err != nil {
			return nil, err
		}
		if err := b.writeAltParts(part, altBoundary, msg.Text, msg.HTML); err != nil {
			return nil, err
		}
	} else {
		ct := "text/plain; charset=utf-8"
		body := msg.Text
		if msg.HTML != "" {
			ct, body = "text/html; charset=utf-8", msg.HTML
		}
		part, err := mw.CreatePart(map[string][]string{
			"Content-Type":              {ct},
			"Content-Transfer-Encoding": {"quoted-printable"},
		})
		if err != nil {
			return nil, err
		}
		if err := writeQP(part, body); err != nil {
			return nil, err
		}
	}

	// Attachment parts.
	for _, att := range msg.Attachments {
		ct := att.ContentType
		if ct == "" {
			ct = mime.TypeByExtension(filepath.Ext(att.Filename))
		}
		if ct == "" {
			ct = "application/octet-stream"
		}
		part, err := mw.CreatePart(map[string][]string{
			"Content-Type":              {ct},
			"Content-Transfer-Encoding": {"base64"},
			"Content-Disposition":       {fmt.Sprintf("attachment; filename=%q", att.Filename)},
		})
		if err != nil {
			return nil, err
		}
		writeBase64(part, att.Content)
	}

	if err := mw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// --- small helpers ---

// headerWriter writes `Key: value\r\n` header lines to a buffer.
type headerWriter struct{ buf *bytes.Buffer }

func (h headerWriter) set(k, v string) {
	h.buf.WriteString(k)
	h.buf.WriteString(": ")
	h.buf.WriteString(v)
	h.buf.WriteString("\r\n")
}

func writeQP(w io.Writer, s string) error {
	qp := quotedprintable.NewWriter(w)
	if _, err := qp.Write([]byte(s)); err != nil {
		return err
	}
	return qp.Close()
}

func writeBase64(w io.Writer, data []byte) {
	enc := base64.StdEncoding.EncodeToString(data)
	// Wrap at 76 chars per RFC 2045.
	for len(enc) > 76 {
		io.WriteString(w, enc[:76])
		io.WriteString(w, "\r\n")
		enc = enc[76:]
	}
	io.WriteString(w, enc)
	io.WriteString(w, "\r\n")
}

// encodeDisplayName renders a display name for a header: RFC 2047 encoded
// when it has non-ASCII, quoted when it has RFC 5322 specials, bare
// otherwise.
func encodeDisplayName(name string) string {
	for _, r := range name {
		if r > 127 {
			return mime.QEncoding.Encode("utf-8", name)
		}
	}
	if strings.ContainsAny(name, "()<>[]:;@\\,.\"") {
		return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(name) + `"`
	}
	return name
}

// isReservedHeader reports whether a custom header would collide with one
// the builder emits itself (case-insensitive).
func isReservedHeader(k string) bool {
	switch strings.ToLower(k) {
	case "from", "to", "cc", "bcc", "reply-to", "subject", "date",
		"mime-version", "content-type", "content-transfer-encoding":
		return true
	}
	return false
}

// validateAddresses parses every recipient so a malformed address fails
// before a transport round-trip.
func validateAddresses(msg Message) error {
	for _, addr := range msg.recipients() {
		if _, err := mail.ParseAddress(addr); err != nil {
			return fmt.Errorf("mail: invalid recipient %q: %w", addr, err)
		}
	}
	return nil
}

// parseAddress returns the bare address from a recipient string, accepting
// both "bob@x.com" and "Bob <bob@x.com>".
func parseAddress(s string) (string, error) {
	a, err := mail.ParseAddress(s)
	if err != nil {
		return "", err
	}
	return a.Address, nil
}
