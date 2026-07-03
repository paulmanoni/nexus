package storage

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"
)

func TestLocalRoundTrip(t *testing.T) {
	t.Parallel()
	d := NewLocalDisk(t.TempDir())
	ctx := context.Background()

	if err := d.Put(ctx, "docs/hello.txt", strings.NewReader("hi there")); err != nil {
		t.Fatalf("Put: %v", err)
	}

	ok, err := d.Exists(ctx, "docs/hello.txt")
	if err != nil || !ok {
		t.Fatalf("Exists: ok=%v err=%v", ok, err)
	}

	rc, err := d.Get(ctx, "docs/hello.txt")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	body, _ := io.ReadAll(rc)
	rc.Close()
	if string(body) != "hi there" {
		t.Fatalf("body = %q", body)
	}

	st, err := d.Stat(ctx, "docs/hello.txt")
	if err != nil || st.Size != 8 {
		t.Fatalf("Stat: %+v err=%v", st, err)
	}

	if err := d.Delete(ctx, "docs/hello.txt"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if ok, _ := d.Exists(ctx, "docs/hello.txt"); ok {
		t.Fatal("still exists after delete")
	}
}

func TestLocalMissingKeyIsErrNotExist(t *testing.T) {
	t.Parallel()
	d := NewLocalDisk(t.TempDir())
	ctx := context.Background()
	if _, err := d.Get(ctx, "nope.txt"); !errors.Is(err, ErrNotExist) {
		t.Fatalf("Get missing: want ErrNotExist, got %v", err)
	}
	if _, err := d.Stat(ctx, "nope.txt"); !errors.Is(err, ErrNotExist) {
		t.Fatalf("Stat missing: want ErrNotExist, got %v", err)
	}
	if err := d.Delete(ctx, "nope.txt"); !errors.Is(err, ErrNotExist) {
		t.Fatalf("Delete missing: want ErrNotExist, got %v", err)
	}
}

// TestLocalRejectsTraversal is the security check: a key that tries to
// escape Root must be refused, never resolving to a path outside Root.
func TestLocalRejectsTraversal(t *testing.T) {
	t.Parallel()
	d := NewLocalDisk(t.TempDir())
	ctx := context.Background()
	for _, key := range []string{"../escape.txt", "a/../../escape.txt", "../../etc/passwd"} {
		if err := d.Put(ctx, key, strings.NewReader("x")); err == nil {
			t.Errorf("Put(%q) should have been rejected", key)
		}
	}
}

func TestLocalList(t *testing.T) {
	t.Parallel()
	d := NewLocalDisk(t.TempDir())
	ctx := context.Background()
	for _, k := range []string{"img/a.png", "img/b.png", "docs/c.txt"} {
		if err := d.Put(ctx, k, bytes.NewReader([]byte("x"))); err != nil {
			t.Fatal(err)
		}
	}
	got, err := d.List(ctx, "img/")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("List(img/) = %d objects, want 2: %+v", len(got), got)
	}
	for _, o := range got {
		if !strings.HasPrefix(o.Key, "img/") {
			t.Errorf("unexpected key %q", o.Key)
		}
	}
}

func TestLocalURL(t *testing.T) {
	t.Parallel()
	// No base URL → error.
	d := NewLocalDisk(t.TempDir())
	if _, err := d.URL("a.png"); err == nil {
		t.Error("URL without PublicBaseURL should error")
	}
	// With base URL → joined.
	d.PublicBaseURL = "https://cdn.example.com/media/"
	got, err := d.URL("avatars/1.png")
	if err != nil || got != "https://cdn.example.com/media/avatars/1.png" {
		t.Fatalf("URL = %q err=%v", got, err)
	}
}

func TestLocalPingAndDescribe(t *testing.T) {
	t.Parallel()
	d := NewLocalDisk(t.TempDir())
	if err := d.Ping(context.Background()); err != nil {
		t.Fatalf("Ping healthy dir: %v", err)
	}
	if d.Driver() != "local" {
		t.Errorf("Driver = %q", d.Driver())
	}
	bad := NewLocalDisk("/no/such/dir/really")
	if err := bad.Ping(context.Background()); err == nil {
		t.Error("Ping on missing dir should error")
	}
}
