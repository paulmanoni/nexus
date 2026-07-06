package mail

import (
	"strings"
	"testing"
	"time"
)

// fixedBuilder returns a mimeBuilder with a deterministic clock and
// boundary so tests can assert on exact bytes.
func fixedBuilder(from Address) mimeBuilder {
	return mimeBuilder{
		from:     from,
		now:      func() time.Time { return time.Date(2026, 7, 6, 12, 0, 0, 0, time.UTC) },
		boundary: func(part string) string { return "BOUNDARY-" + part },
	}
}

func TestBuild_PlainText(t *testing.T) {
	b := fixedBuilder(Address{Name: "Example", Email: "no-reply@example.com"})
	raw, err := b.build(Message{
		To:      []string{"bob@example.com"},
		Subject: "Hello",
		Text:    "Hi there",
	})
	if err != nil {
		t.Fatal(err)
	}
	s := string(raw)
	for _, want := range []string{
		"From: Example <no-reply@example.com>\r\n",
		"To: bob@example.com\r\n",
		"Subject: Hello\r\n",
		"Date: Mon, 06 Jul 2026 12:00:00 +0000\r\n",
		"MIME-Version: 1.0\r\n",
		"Content-Type: text/plain; charset=utf-8\r\n",
		"Content-Transfer-Encoding: quoted-printable\r\n",
		"Hi there",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("plain message missing %q\n---\n%s", want, s)
		}
	}
}

func TestBuild_HTMLOnly(t *testing.T) {
	b := fixedBuilder(Address{Email: "a@b.com"})
	raw, _ := b.build(Message{To: []string{"x@y.com"}, HTML: "<p>Hi</p>"})
	s := string(raw)
	if !strings.Contains(s, "Content-Type: text/html; charset=utf-8") {
		t.Errorf("html message should be text/html:\n%s", s)
	}
}

func TestBuild_Alternative(t *testing.T) {
	b := fixedBuilder(Address{Email: "a@b.com"})
	raw, _ := b.build(Message{
		To:   []string{"x@y.com"},
		Text: "plain",
		HTML: "<p>rich</p>",
	})
	s := string(raw)
	if !strings.Contains(s, "Content-Type: multipart/alternative; boundary=BOUNDARY-alt") {
		t.Errorf("text+html should be multipart/alternative:\n%s", s)
	}
	// Both parts present, text before html.
	ti := strings.Index(s, "text/plain")
	hi := strings.Index(s, "text/html")
	if ti < 0 || hi < 0 || ti > hi {
		t.Errorf("expected text part before html part (t=%d h=%d)", ti, hi)
	}
	if !strings.Contains(s, "--BOUNDARY-alt--") {
		t.Errorf("alternative boundary not closed:\n%s", s)
	}
}

func TestBuild_MixedWithAttachment(t *testing.T) {
	b := fixedBuilder(Address{Email: "a@b.com"})
	raw, _ := b.build(Message{
		To:      []string{"x@y.com"},
		Text:    "see attached",
		Subject: "Report",
		Attachments: []Attachment{
			{Filename: "report.txt", Content: []byte("col1,col2\n1,2\n")},
		},
	})
	s := string(raw)
	for _, want := range []string{
		"Content-Type: multipart/mixed; boundary=BOUNDARY-mixed",
		"Content-Disposition: attachment; filename=\"report.txt\"",
		"Content-Transfer-Encoding: base64",
		"--BOUNDARY-mixed--",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("mixed message missing %q\n---\n%s", want, s)
		}
	}
	// text/plain guessed for the .txt attachment.
	if !strings.Contains(s, "text/plain") {
		t.Errorf("attachment content-type should be guessed:\n%s", s)
	}
}

func TestBuild_MixedAlternativeWithAttachment(t *testing.T) {
	b := fixedBuilder(Address{Email: "a@b.com"})
	raw, _ := b.build(Message{
		To:          []string{"x@y.com"},
		Text:        "plain",
		HTML:        "<p>rich</p>",
		Attachments: []Attachment{{Filename: "a.bin", Content: []byte{0, 1, 2}}},
	})
	s := string(raw)
	if !strings.Contains(s, "multipart/mixed; boundary=BOUNDARY-mixed") {
		t.Errorf("want mixed outer:\n%s", s)
	}
	if !strings.Contains(s, "multipart/alternative; boundary=BOUNDARY-alt") {
		t.Errorf("want nested alternative:\n%s", s)
	}
	if !strings.Contains(s, "application/octet-stream") {
		t.Errorf("unknown ext should default to octet-stream:\n%s", s)
	}
}

func TestBuild_EncodesUnicodeSubjectAndName(t *testing.T) {
	b := fixedBuilder(Address{Name: "Café", Email: "a@b.com"})
	raw, _ := b.build(Message{To: []string{"x@y.com"}, Subject: "Réservé", Text: "hi"})
	s := string(raw)
	if !strings.Contains(s, "Subject: =?utf-8?q?") {
		t.Errorf("unicode subject should be RFC2047-encoded:\n%s", s)
	}
	if !strings.Contains(s, "From: =?utf-8?q?") {
		t.Errorf("unicode display name should be RFC2047-encoded:\n%s", s)
	}
}

func TestBuild_SkipsReservedCustomHeaders(t *testing.T) {
	b := fixedBuilder(Address{Email: "a@b.com"})
	raw, _ := b.build(Message{
		To:      []string{"x@y.com"},
		Text:    "hi",
		Headers: map[string]string{"X-Custom": "yes", "Subject": "HIJACK"},
	})
	s := string(raw)
	if !strings.Contains(s, "X-Custom: yes") {
		t.Errorf("custom header dropped:\n%s", s)
	}
	if strings.Contains(s, "HIJACK") {
		t.Errorf("reserved header override should be skipped:\n%s", s)
	}
}

func TestBuild_MessageFromOverridesDefault(t *testing.T) {
	b := fixedBuilder(Address{Email: "default@b.com"})
	raw, _ := b.build(Message{
		From: Address{Email: "override@b.com"},
		To:   []string{"x@y.com"},
		Text: "hi",
	})
	if !strings.Contains(string(raw), "From: override@b.com") {
		t.Errorf("Message.From should override the default sender:\n%s", raw)
	}
}

func TestValidateAddresses(t *testing.T) {
	if err := validateAddresses(Message{To: []string{"good@x.com"}}); err != nil {
		t.Errorf("valid address rejected: %v", err)
	}
	if err := validateAddresses(Message{To: []string{"not-an-email"}}); err == nil {
		t.Error("invalid address accepted")
	}
}

func TestEncodeDisplayName(t *testing.T) {
	cases := map[string]string{
		"Alice":     "Alice",
		"Doe, John": `"Doe, John"`,
		"a@b":       `"a@b"`,
	}
	for in, want := range cases {
		if got := encodeDisplayName(in); got != want {
			t.Errorf("encodeDisplayName(%q) = %q, want %q", in, got, want)
		}
	}
	if got := encodeDisplayName("Café"); !strings.HasPrefix(got, "=?utf-8?q?") {
		t.Errorf("unicode name should be encoded, got %q", got)
	}
}
