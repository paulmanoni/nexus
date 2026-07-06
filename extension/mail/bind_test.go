package mail

import (
	"testing"
	"time"
)

func TestBuildMailer_DriverSelection(t *testing.T) {
	// Default (empty) → log.
	m, err := buildMailer(Config{FromAddress: "a@b.com"})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := m.(*LogMailer); !ok {
		t.Errorf("empty driver should be LogMailer, got %T", m)
	}

	// Explicit log.
	m, _ = buildMailer(Config{Driver: "log"})
	if _, ok := m.(*LogMailer); !ok {
		t.Errorf("driver=log should be LogMailer, got %T", m)
	}

	// SMTP.
	m, err = buildMailer(Config{Driver: "smtp", Host: "smtp.example.com", FromAddress: "a@b.com"})
	if err != nil {
		t.Fatal(err)
	}
	sm, ok := m.(*SMTPMailer)
	if !ok {
		t.Fatalf("driver=smtp should be SMTPMailer, got %T", m)
	}
	if sm.Encryption != EncryptionStartTLS {
		t.Errorf("default encryption = %q, want starttls", sm.Encryption)
	}
	if sm.Port != 587 {
		t.Errorf("default starttls port = %d, want 587", sm.Port)
	}
}

func TestBuildMailer_TLSPortDefault(t *testing.T) {
	m, _ := buildMailer(Config{Driver: "smtp", Host: "h", Encryption: "tls"})
	if m.(*SMTPMailer).Port != 465 {
		t.Errorf("tls default port = %d, want 465", m.(*SMTPMailer).Port)
	}
}

func TestBuildMailer_Errors(t *testing.T) {
	if _, err := buildMailer(Config{Driver: "smtp"}); err == nil {
		t.Error("smtp without Host should error")
	}
	if _, err := buildMailer(Config{Driver: "smtp", Host: "h", Encryption: "bogus"}); err == nil {
		t.Error("unknown encryption should error")
	}
	if _, err := buildMailer(Config{Driver: "carrier-pigeon"}); err == nil {
		t.Error("unknown driver should error")
	}
}

func TestBuildMailer_CarriesFromAndTimeout(t *testing.T) {
	m, _ := buildMailer(Config{
		Driver:      "smtp",
		Host:        "h",
		FromAddress: "no-reply@x.com",
		FromName:    "X",
		Timeout:     5 * time.Second,
	})
	sm := m.(*SMTPMailer)
	if sm.From.Email != "no-reply@x.com" || sm.From.Name != "X" {
		t.Errorf("From not carried: %+v", sm.From)
	}
	if sm.Timeout != 5*time.Second {
		t.Errorf("Timeout not carried: %v", sm.Timeout)
	}
}

// Uploads-style embed for the field-index helper.
type testMailer struct{ *Manager }

func TestEmbeddedManagerField(t *testing.T) {
	if idx := embeddedManagerField[testMailer](); idx != 0 {
		t.Errorf("embedded *Manager field index = %d, want 0", idx)
	}
}

func TestEmbeddedManagerField_PanicsWithoutEmbed(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("expected panic for a type not embedding *Manager")
		}
	}()
	type bad struct{ X int }
	embeddedManagerField[bad]()
}
