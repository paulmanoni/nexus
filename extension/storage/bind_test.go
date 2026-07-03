package storage

import (
	"context"
	"strings"
	"testing"

	"github.com/paulmanoni/nexus"
)

func TestBuildDiskValidation(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		cfg     Config
		wantErr string // "" = expect success
		driver  string
	}{
		{"local ok", Config{Driver: "local", Root: "/tmp/x"}, "", "local"},
		{"local default driver", Config{Root: "/tmp/x"}, "", "local"},
		{"local missing root", Config{Driver: "local"}, "requires Root", ""},
		{"s3 ok", Config{Driver: "s3", Bucket: "b", Region: "r", AccessKey: "k", SecretKey: "s"}, "", "s3"},
		{"s3 missing bucket", Config{Driver: "s3", Region: "r", AccessKey: "k", SecretKey: "s"}, "Bucket and Region", ""},
		{"s3 missing creds", Config{Driver: "s3", Bucket: "b", Region: "r"}, "AccessKey and SecretKey", ""},
		{"unknown driver", Config{Driver: "gcs"}, "unknown driver", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			disk, err := buildDisk(tc.cfg)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("want error containing %q, got %v", tc.wantErr, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if d, ok := disk.(described); !ok || d.Driver() != tc.driver {
				t.Fatalf("driver mismatch: got %v", disk)
			}
		})
	}
}

// Uploads is the user-style typed handle for the e2e Bind test.
type Uploads struct{ *Manager }

// TestBindEndToEnd wires a local disk through a real nexus app and proves
// the injected handle round-trips a file and registers a resource.
func TestBindEndToEnd(t *testing.T) {
	dir := t.TempDir()
	var got *Uploads
	app, stop, err := nexus.InProcess(nexus.Config{},
		Bind[Uploads]("uploads", func() Config {
			return Config{Driver: "local", Root: dir}
		}, WithDefault(), WithDescription("test uploads")),
		nexus.Invoke(func(u *Uploads) { got = u }),
	)
	if err != nil {
		t.Fatalf("InProcess: %v", err)
	}
	defer stop(context.Background())

	if got == nil {
		t.Fatal("Uploads was not injected")
	}
	ctx := context.Background()
	if err := got.Put(ctx, "hi.txt", strings.NewReader("data")); err != nil {
		t.Fatalf("Put via injected handle: %v", err)
	}
	if ok, _ := got.Exists(ctx, "hi.txt"); !ok {
		t.Fatal("file not stored")
	}

	// The disk should be registered as a dashboard resource.
	found := false
	for _, r := range app.Registry().Resources() {
		if r.Name == "uploads" && string(r.Kind) == "storage" {
			found = true
		}
	}
	if !found {
		t.Error("uploads storage resource was not registered")
	}
}
