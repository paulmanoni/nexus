package storage

import (
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeS3 is a minimal path-style S3-compatible server backed by an
// in-memory map, enough to exercise the S3Disk HTTP plumbing end to end.
type fakeS3 struct {
	mu      sync.Mutex
	objects map[string][]byte
	bucket  string
}

func newFakeS3(bucket string) *fakeS3 {
	return &fakeS3{objects: map[string][]byte{}, bucket: bucket}
}

func (f *fakeS3) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Every request must be SigV4-signed.
	if !strings.HasPrefix(r.Header.Get("Authorization"), "AWS4-HMAC-SHA256") {
		http.Error(w, "missing SigV4 auth", http.StatusForbidden)
		return
	}
	// Path is /{bucket}/{key...} (path-style).
	trimmed := strings.TrimPrefix(r.URL.Path, "/"+f.bucket)
	key := strings.TrimPrefix(trimmed, "/")

	// List: GET /{bucket}?list-type=2
	if r.Method == http.MethodGet && r.URL.Query().Get("list-type") == "2" {
		f.list(w, r)
		return
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	switch r.Method {
	case http.MethodPut:
		body, _ := io.ReadAll(r.Body)
		f.objects[key] = body
		w.WriteHeader(http.StatusOK)
	case http.MethodGet:
		b, ok := f.objects[key]
		if !ok {
			http.Error(w, "<Error><Code>NoSuchKey</Code></Error>", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Length", strconv.Itoa(len(b)))
		w.Write(b)
	case http.MethodHead:
		b, ok := f.objects[key]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Length", strconv.Itoa(len(b)))
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("ETag", `"deadbeef"`)
		w.Header().Set("Last-Modified", time.Now().UTC().Format(http.TimeFormat))
		w.WriteHeader(http.StatusOK)
	case http.MethodDelete:
		delete(f.objects, key)
		w.WriteHeader(http.StatusNoContent)
	default:
		http.Error(w, "unsupported", http.StatusMethodNotAllowed)
	}
}

func (f *fakeS3) list(w http.ResponseWriter, r *http.Request) {
	prefix := r.URL.Query().Get("prefix")
	f.mu.Lock()
	defer f.mu.Unlock()
	type content struct {
		Key          string    `xml:"Key"`
		Size         int64     `xml:"Size"`
		ETag         string    `xml:"ETag"`
		LastModified time.Time `xml:"LastModified"`
	}
	var res struct {
		XMLName  xml.Name  `xml:"ListBucketResult"`
		Contents []content `xml:"Contents"`
	}
	for k, v := range f.objects {
		if strings.HasPrefix(k, prefix) {
			res.Contents = append(res.Contents, content{Key: k, Size: int64(len(v)), ETag: `"x"`, LastModified: time.Now().UTC()})
		}
	}
	w.Header().Set("Content-Type", "application/xml")
	fmt.Fprint(w, xml.Header)
	xml.NewEncoder(w).Encode(res)
}

func newTestS3(t *testing.T) (*S3Disk, *fakeS3) {
	t.Helper()
	fake := newFakeS3("mybucket")
	srv := httptest.NewServer(fake)
	t.Cleanup(srv.Close)
	d := &S3Disk{
		Bucket:    "mybucket",
		Region:    "us-east-1",
		Endpoint:  srv.URL, // path-style, http
		AccessKey: "AK",
		SecretKey: "SK",
	}
	return d, fake
}

func TestS3RoundTrip(t *testing.T) {
	t.Parallel()
	d, _ := newTestS3(t)
	ctx := context.Background()

	if err := d.Put(ctx, "docs/a.txt", strings.NewReader("payload"), WithContentType("text/plain")); err != nil {
		t.Fatalf("Put: %v", err)
	}
	ok, err := d.Exists(ctx, "docs/a.txt")
	if err != nil || !ok {
		t.Fatalf("Exists: ok=%v err=%v", ok, err)
	}
	rc, err := d.Get(ctx, "docs/a.txt")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	body, _ := io.ReadAll(rc)
	rc.Close()
	if string(body) != "payload" {
		t.Fatalf("body = %q", body)
	}
	st, err := d.Stat(ctx, "docs/a.txt")
	if err != nil || st.ETag != "deadbeef" {
		t.Fatalf("Stat: %+v err=%v", st, err)
	}
	if err := d.Delete(ctx, "docs/a.txt"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if ok, _ := d.Exists(ctx, "docs/a.txt"); ok {
		t.Fatal("still exists after delete")
	}
}

func TestS3PutStreamingKnownSize(t *testing.T) {
	t.Parallel()
	d, fake := newTestS3(t)
	ctx := context.Background()
	// WithSize avoids buffering — the body streams with a set Content-Length.
	if err := d.Put(ctx, "big.bin", strings.NewReader("0123456789"), WithSize(10)); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if got := string(fake.objects["big.bin"]); got != "0123456789" {
		t.Fatalf("stored %q", got)
	}
}

func TestS3GetMissingIsErrNotExist(t *testing.T) {
	t.Parallel()
	d, _ := newTestS3(t)
	if _, err := d.Get(context.Background(), "ghost.txt"); !errors.Is(err, ErrNotExist) {
		t.Fatalf("want ErrNotExist, got %v", err)
	}
}

func TestS3List(t *testing.T) {
	t.Parallel()
	d, _ := newTestS3(t)
	ctx := context.Background()
	for _, k := range []string{"u/1.png", "u/2.png", "other.txt"} {
		if err := d.Put(ctx, k, strings.NewReader("x")); err != nil {
			t.Fatal(err)
		}
	}
	got, err := d.List(ctx, "u/")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("List(u/) = %d, want 2: %+v", len(got), got)
	}
}

func TestS3SignedURL(t *testing.T) {
	t.Parallel()
	d := NewS3Disk("b", "us-east-1", "AK", "SK")
	u, err := d.SignedURL(context.Background(), "path/to/x.png", 10*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(u, "b.s3.us-east-1.amazonaws.com/path/to/x.png") {
		t.Errorf("host/path wrong: %s", u)
	}
	if !strings.Contains(u, "X-Amz-Signature=") || !strings.Contains(u, "X-Amz-Expires=600") {
		t.Errorf("presign params missing: %s", u)
	}
}

func TestS3VirtualHostedURLs(t *testing.T) {
	t.Parallel()
	d := NewS3Disk("mybucket", "eu-west-1", "AK", "SK")
	full, host := d.objectURL("a/b.png")
	if host != "mybucket.s3.eu-west-1.amazonaws.com" {
		t.Errorf("host = %q", host)
	}
	if full != "https://mybucket.s3.eu-west-1.amazonaws.com/a/b.png" {
		t.Errorf("url = %q", full)
	}
}
