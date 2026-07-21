package mail

import (
	"testing"
	"time"

	"github.com/paulmanoni/nexus"
)

// TestConfigFromTOML reads a [mail.<name>] block into a Config, including the
// int port and duration timeout conversions.
func TestConfigFromTOML(t *testing.T) {
	nexus.ClearConfigStoreForTest()
	t.Cleanup(nexus.ClearConfigStoreForTest)
	nexus.InstallConfigStore(map[string]any{
		"mail": map[string]any{
			"smtp": map[string]any{
				"driver":       "smtp",
				"host":         "smtp.example.com",
				"port":         2525,
				"encryption":   "starttls",
				"from_address": "no-reply@example.com",
				"timeout":      "10s",
			},
		},
	}, "test")

	c := configFromTOML("smtp")
	if c.Driver != "smtp" || c.Host != "smtp.example.com" {
		t.Fatalf("unexpected config: %+v", c)
	}
	if c.Port != 2525 {
		t.Errorf("Port = %d, want 2525", c.Port)
	}
	if c.Encryption != "starttls" {
		t.Errorf("Encryption = %q, want starttls", c.Encryption)
	}
	if c.FromAddress != "no-reply@example.com" {
		t.Errorf("FromAddress = %q", c.FromAddress)
	}
	if c.Timeout != 10*time.Second {
		t.Errorf("Timeout = %v, want 10s", c.Timeout)
	}
}
