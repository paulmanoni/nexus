package rabbit

import "testing"

// TestConnectionDetails exercises the URL → dashboard-details
// extractor. The dashboard renders this map on the resource card
// for the auto-registered queue, so a regression that leaks the
// password into details would be a real security bug.
func TestConnectionDetails(t *testing.T) {
	cases := []struct {
		name string
		url  string
		want map[string]any
	}{
		{
			name: "full URL with password",
			url:  "amqp://guest:secret@broker.local:5672/myvhost",
			want: map[string]any{
				"engine": "rabbitmq",
				"host":   "broker.local:5672",
				"user":   "guest",
				"vhost":  "/myvhost",
			},
		},
		{
			name: "no vhost",
			url:  "amqp://user:pwd@host:5672/",
			want: map[string]any{
				"engine": "rabbitmq",
				"host":   "host:5672",
				"user":   "user",
			},
		},
		{
			name: "no auth",
			url:  "amqp://host:5672",
			want: map[string]any{
				"engine": "rabbitmq",
				"host":   "host:5672",
			},
		},
		{
			name: "malformed",
			url:  "%%%not a url",
			want: map[string]any{"engine": "rabbitmq"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := connectionDetails(c.url)
			if len(got) != len(c.want) {
				t.Fatalf("size mismatch: got %d (%v) want %d (%v)", len(got), got, len(c.want), c.want)
			}
			for k, v := range c.want {
				if got[k] != v {
					t.Errorf("%s: got %v want %v", k, got[k], v)
				}
			}
			// Password must never appear under any key.
			for k, v := range got {
				if s, ok := v.(string); ok && (s == "secret" || s == "pwd") {
					t.Errorf("password leaked into details[%s]: %v", k, s)
				}
			}
		})
	}
}

// TestHealthy_ClosedTransport confirms Healthy reports false after
// Close, even if no real connection was opened. Defensive: a code
// path that called Close on a partially-constructed transport (dial
// failed mid-init) shouldn't make the dashboard claim green.
func TestHealthy_ClosedTransport(t *testing.T) {
	tr := &Transport{} // zero-value; no conn
	if tr.Healthy() {
		t.Error("zero-value transport should not be Healthy")
	}
	_ = tr.Close()
	if tr.Healthy() {
		t.Error("closed transport should not be Healthy")
	}
}