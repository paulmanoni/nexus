package mail

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeSMTP is a minimal in-process SMTP server for tests: it speaks just
// enough of the protocol for SMTPMailer to complete a plaintext send and
// captures the envelope + DATA payload.
type fakeSMTP struct {
	ln   net.Listener
	mu   sync.Mutex
	from string
	rcpt []string
	data string
	done chan struct{}
}

func newFakeSMTP(t *testing.T) *fakeSMTP {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeSMTP{ln: ln, done: make(chan struct{})}
	go f.serve()
	t.Cleanup(func() { ln.Close() })
	return f
}

func (f *fakeSMTP) hostPort(t *testing.T) (string, int) {
	t.Helper()
	host, portStr, _ := net.SplitHostPort(f.ln.Addr().String())
	port, _ := strconv.Atoi(portStr)
	return host, port
}

func (f *fakeSMTP) serve() {
	conn, err := f.ln.Accept()
	if err != nil {
		return
	}
	defer conn.Close()
	defer close(f.done)

	r := bufio.NewReader(conn)
	w := bufio.NewWriter(conn)
	writeLine := func(s string) { w.WriteString(s + "\r\n"); w.Flush() }

	writeLine("220 fake ESMTP")
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return
		}
		cmd := strings.TrimSpace(line)
		upper := strings.ToUpper(cmd)
		switch {
		case strings.HasPrefix(upper, "EHLO"), strings.HasPrefix(upper, "HELO"):
			writeLine("250-fake greets you")
			writeLine("250 OK")
		case strings.HasPrefix(upper, "MAIL FROM:"):
			f.mu.Lock()
			f.from = strings.TrimPrefix(cmd[len("MAIL FROM:"):], " ")
			f.mu.Unlock()
			writeLine("250 OK")
		case strings.HasPrefix(upper, "RCPT TO:"):
			f.mu.Lock()
			f.rcpt = append(f.rcpt, strings.TrimPrefix(cmd[len("RCPT TO:"):], " "))
			f.mu.Unlock()
			writeLine("250 OK")
		case upper == "DATA":
			writeLine("354 end with .")
			var b strings.Builder
			for {
				dl, derr := r.ReadString('\n')
				if derr != nil {
					return
				}
				if strings.TrimRight(dl, "\r\n") == "." {
					break
				}
				b.WriteString(dl)
			}
			f.mu.Lock()
			f.data = b.String()
			f.mu.Unlock()
			writeLine("250 queued")
		case upper == "NOOP":
			writeLine("250 OK")
		case upper == "QUIT":
			writeLine("221 bye")
			return
		default:
			writeLine("250 OK")
		}
	}
}

func TestSMTPMailer_SendsOverPlaintext(t *testing.T) {
	f := newFakeSMTP(t)
	host, port := f.hostPort(t)

	m := &SMTPMailer{
		Host:       host,
		Port:       port,
		Encryption: EncryptionNone,
		From:       Address{Name: "App", Email: "app@example.com"},
		Timeout:    5 * time.Second,
		now:        func() time.Time { return time.Date(2026, 7, 6, 12, 0, 0, 0, time.UTC) },
		boundary:   func(p string) string { return "B-" + p },
	}

	err := m.Send(context.Background(), Message{
		To:      []string{"Bob <bob@example.com>"},
		Subject: "Hi",
		Text:    "hello",
	})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}

	select {
	case <-f.done:
	case <-time.After(3 * time.Second):
		t.Fatal("server did not finish")
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	if !strings.Contains(f.from, "app@example.com") {
		t.Errorf("MAIL FROM = %q, want app@example.com", f.from)
	}
	if len(f.rcpt) != 1 || !strings.Contains(f.rcpt[0], "bob@example.com") {
		t.Errorf("RCPT = %v, want bob@example.com (bare, no display name)", f.rcpt)
	}
	for _, want := range []string{"Subject: Hi", "From: App <app@example.com>", "hello"} {
		if !strings.Contains(f.data, want) {
			t.Errorf("DATA missing %q\n---\n%s", want, f.data)
		}
	}
}

func TestSMTPMailer_NoRecipients(t *testing.T) {
	m := &SMTPMailer{Host: "unused", From: Address{Email: "a@b.com"}}
	if err := m.Send(context.Background(), Message{Subject: "x"}); err != ErrNoRecipients {
		t.Errorf("want ErrNoRecipients, got %v", err)
	}
}

func TestSMTPMailer_MissingFrom(t *testing.T) {
	m := &SMTPMailer{Host: "unused"}
	err := m.Send(context.Background(), Message{To: []string{"a@b.com"}, Text: "hi"})
	if err == nil || !strings.Contains(err.Error(), "From") {
		t.Errorf("want a missing-From error, got %v", err)
	}
}

func TestSMTPMailer_Ping(t *testing.T) {
	f := newFakeSMTP(t)
	host, port := f.hostPort(t)
	m := &SMTPMailer{Host: host, Port: port, Encryption: EncryptionNone, Timeout: 5 * time.Second}
	if err := m.Ping(context.Background()); err != nil {
		t.Errorf("Ping over reachable server: %v", err)
	}
}

func TestSMTPMailer_DialError(t *testing.T) {
	// Nothing listening on this port → dial error.
	m := &SMTPMailer{Host: "127.0.0.1", Port: 1, Encryption: EncryptionNone, Timeout: time.Second}
	err := m.Send(context.Background(), Message{To: []string{"a@b.com"}, From: Address{Email: "x@y.com"}, Text: "hi"})
	if err == nil || !strings.Contains(err.Error(), "dial") {
		t.Errorf("want a dial error, got %v", err)
	}
}

func TestSMTPMailer_Described(t *testing.T) {
	m := &SMTPMailer{Host: "smtp.x.com", Port: 587, Username: "u", Encryption: EncryptionStartTLS,
		From: Address{Email: "a@b.com"}}
	d := m.MailDetails()
	if d["host"] != "smtp.x.com" || d["encryption"] != "starttls" || d["auth"] != true {
		t.Errorf("MailDetails = %v", d)
	}
	if fmt.Sprint(d["port"]) != "587" {
		t.Errorf("port detail = %v", d["port"])
	}
}
