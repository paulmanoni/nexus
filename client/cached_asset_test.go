package client

import (
	"bytes"
	"compress/gzip"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

// TestCachedAsset_HashesAndCompresses asserts the basic invariants:
// every non-empty body produces a strong ETag and a non-empty gzip
// copy when compression actually shrinks; identical bodies produce
// identical ETags so the manifest survives marshal-shuffles without
// invalidating browser caches.
func TestCachedAsset_HashesAndCompresses(t *testing.T) {
	body := bytes.Repeat([]byte("hello world "), 200) // ~2.4 KiB, gzips well
	a := newCachedAsset(body)

	if a.etag == "" {
		t.Fatal("etag should be non-empty")
	}
	if !strings.HasPrefix(a.etag, `"`) || !strings.HasSuffix(a.etag, `"`) {
		t.Fatalf("etag should be quoted, got %q", a.etag)
	}
	if a.gz == nil {
		t.Fatal("gz should be set when compression helps")
	}
	if len(a.gz) >= len(a.body) {
		t.Fatalf("gz=%d body=%d — compression should shrink", len(a.gz), len(a.body))
	}

	// Stable hash: rebuilding from the same bytes yields the same ETag.
	a2 := newCachedAsset(append([]byte(nil), body...))
	if a.etag != a2.etag {
		t.Errorf("etags should match: %q vs %q", a.etag, a2.etag)
	}
}

// TestCachedAsset_SkipsGzipWhenUnhelpful guards against a pathological
// case: tiny bodies where the gzip framing exceeds the savings. We
// store gz=nil and the serve path falls through to the raw body.
func TestCachedAsset_SkipsGzipWhenUnhelpful(t *testing.T) {
	a := newCachedAsset([]byte("ok"))
	if a.gz != nil {
		t.Errorf("gz should be nil for tiny incompressible body, got %d bytes", len(a.gz))
	}
}

// TestServeCachedAsset_ReturnsETag confirms the response header path.
// First request: 200 + ETag. Browser stashes both. We don't assert
// the body here — that's covered by ReturnsCompressed.
func TestServeCachedAsset_ReturnsETag(t *testing.T) {
	body := bytes.Repeat([]byte("a"), 1024)
	a := newCachedAsset(body)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodGet, "/x", nil)

	serveCachedAsset(c, "application/json", a)

	if w.Code != http.StatusOK {
		t.Fatalf("status=%d want 200", w.Code)
	}
	if got := w.Header().Get("ETag"); got != a.etag {
		t.Errorf("etag header=%q want %q", got, a.etag)
	}
	if got := w.Header().Get("Cache-Control"); got != "no-cache, must-revalidate" {
		t.Errorf("cache-control=%q", got)
	}
}

// TestServeCachedAsset_Returns304 covers the conditional-GET path:
// when the client sends If-None-Match with our ETag, we MUST return
// 304 and an empty body. This is where the bandwidth savings come
// from — a cached SDK init becomes a header-only round-trip. Routed
// through a real engine because gin.CreateTestContext doesn't fully
// wire the request shadow that c.GetHeader reads from.
func TestServeCachedAsset_Returns304(t *testing.T) {
	body := bytes.Repeat([]byte("a"), 1024)
	a := newCachedAsset(body)

	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.GET("/x", func(c *gin.Context) {
		serveCachedAsset(c, "application/json", a)
	})

	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	req.Header.Set("If-None-Match", a.etag)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusNotModified {
		t.Fatalf("status=%d want 304", w.Code)
	}
	if w.Body.Len() != 0 {
		t.Errorf("304 body should be empty, got %d bytes", w.Body.Len())
	}
}

// TestServeCachedAsset_ReturnsCompressed exercises the gzip path:
// Accept-Encoding: gzip with no If-None-Match → 200 + gzip body +
// Content-Encoding header. The body must round-trip through a gzip
// decoder back to the original.
func TestServeCachedAsset_ReturnsCompressed(t *testing.T) {
	body := bytes.Repeat([]byte("hello "), 500)
	a := newCachedAsset(body)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	req.Header.Set("Accept-Encoding", "gzip, deflate")
	c.Request = req

	serveCachedAsset(c, "application/json", a)

	if w.Code != http.StatusOK {
		t.Fatalf("status=%d want 200", w.Code)
	}
	if got := w.Header().Get("Content-Encoding"); got != "gzip" {
		t.Fatalf("content-encoding=%q want gzip", got)
	}
	gr, err := gzip.NewReader(w.Body)
	if err != nil {
		t.Fatalf("gzip reader: %v", err)
	}
	defer gr.Close()
	out, err := io.ReadAll(gr)
	if err != nil {
		t.Fatalf("decompress: %v", err)
	}
	if !bytes.Equal(out, body) {
		t.Errorf("decompressed body mismatch")
	}
}

// TestServeCachedAsset_PlainWhenNoGzip confirms the fallback: client
// without Accept-Encoding: gzip gets the raw body verbatim. This is
// the path stale http clients (and curl --no-compress) hit.
func TestServeCachedAsset_PlainWhenNoGzip(t *testing.T) {
	body := bytes.Repeat([]byte("a"), 1024)
	a := newCachedAsset(body)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodGet, "/x", nil) // no Accept-Encoding

	serveCachedAsset(c, "application/json", a)

	if got := w.Header().Get("Content-Encoding"); got != "" {
		t.Errorf("content-encoding should be empty for non-gzip clients, got %q", got)
	}
	if !bytes.Equal(w.Body.Bytes(), body) {
		t.Errorf("body should be the raw asset")
	}
}

func TestEtagMatches(t *testing.T) {
	cases := []struct {
		ifNoneMatch, etag string
		want              bool
	}{
		{`"abc"`, `"abc"`, true},
		{`"abc"`, `"def"`, false},
		{`"abc","def"`, `"def"`, true},
		{`"abc", "def"`, `"def"`, true},
		{`*`, `"anything"`, true},
		{``, `"abc"`, false},
	}
	for _, tc := range cases {
		if got := etagMatches(tc.ifNoneMatch, tc.etag); got != tc.want {
			t.Errorf("etagMatches(%q, %q)=%v want %v", tc.ifNoneMatch, tc.etag, got, tc.want)
		}
	}
}