package client

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"strings"

	"github.com/paulmanoni/nexus/httpx"
)

// cachedAsset bundles a static byte body with the precomputed
// artifacts the HTTP layer needs for cheap conditional + compressed
// responses: a strong ETag derived from the body's content, and a
// gzip-encoded copy. Built once per build() (or once at package init
// for the embedded JS files), reused for every request.
//
// gz is nil when compression didn't shrink the body — small / already-
// compressed payloads pay the gzip framing tax for no benefit, so we
// skip compression for them. Tested by length comparison rather than
// a hardcoded threshold so the framework adapts to whatever shape the
// embedded SDK + manifest happen to be.
type cachedAsset struct {
	body []byte
	etag string
	gz   []byte
}

// newCachedAsset hashes body, gzips it at BestCompression (build is
// once-per-mount, so a few ms here saves bandwidth on every request
// for the lifetime of the process), and returns the bundle. Empty
// body → zero asset; the serve path treats that as "fall through to
// the legacy direct write" so a half-built handler can never serve
// stale ETags.
func newCachedAsset(body []byte) cachedAsset {
	if len(body) == 0 {
		return cachedAsset{}
	}
	sum := sha256.Sum256(body)
	// 8 bytes (16 hex chars) of SHA-256 is plenty of entropy for an
	// ETag — collisions on a single host's manifest are not a thing
	// at this size, and the shorter header trims a handful of bytes
	// off every response.
	etag := `"` + hex.EncodeToString(sum[:8]) + `"`

	var buf bytes.Buffer
	gw, _ := gzip.NewWriterLevel(&buf, gzip.BestCompression)
	_, _ = gw.Write(body)
	_ = gw.Close()
	var gz []byte
	if buf.Len() < len(body) {
		gz = buf.Bytes()
	}
	return cachedAsset{body: body, etag: etag, gz: gz}
}

// serveCachedAsset writes a 304 when If-None-Match matches the asset's
// ETag, otherwise writes the body — gzip-encoded when the client's
// Accept-Encoding allows it and the precomputed gz copy is shorter
// than the raw body. Cache-Control stays at "no-cache, must-
// revalidate": the browser MAY store the body but MUST revalidate
// before reuse, which is exactly the contract ETag was built for.
//
// Falls through to a plain c.Data when asset is zero — keeps the
// old behavior for code paths that haven't migrated to cachedAsset
// yet (none today, but the seam matters during refactors).
func serveCachedAsset(c *httpx.Ctx, contentType string, a cachedAsset) {
	if len(a.body) == 0 {
		c.Header("Cache-Control", "no-cache, no-store, must-revalidate")
		c.Status(http.StatusOK)
		return
	}
	c.Header("ETag", a.etag)
	c.Header("Cache-Control", "no-cache, must-revalidate")
	c.Header("Vary", "Accept-Encoding")

	if match := c.GetHeader("If-None-Match"); match != "" && etagMatches(match, a.etag) {
		c.Status(http.StatusNotModified)
		return
	}

	if a.gz != nil && acceptsGzip(c) {
		c.Header("Content-Encoding", "gzip")
		c.Data(http.StatusOK, contentType, a.gz)
		return
	}
	c.Data(http.StatusOK, contentType, a.body)
}

// etagMatches implements the subset of RFC 7232 §3.2 we actually
// need: a literal "*" wildcard, or a comma-separated list of strong
// ETags one of which exactly matches our asset's tag. We don't emit
// weak ETags (W/-prefixed) so we don't need to handle weak-comparison
// rules — exact string match is sufficient.
func etagMatches(ifNoneMatch, etag string) bool {
	ifNoneMatch = strings.TrimSpace(ifNoneMatch)
	if ifNoneMatch == "*" {
		return true
	}
	for _, e := range strings.Split(ifNoneMatch, ",") {
		if strings.TrimSpace(e) == etag {
			return true
		}
	}
	return false
}

// acceptsGzip is a coarse check — good enough since we control both
// ends and don't need to honor q-values. Browsers and curl/HTTP
// libraries that opt into gzip always include the literal "gzip" in
// Accept-Encoding; clients that explicitly disable it ("identity")
// won't, and we'll fall through to the uncompressed body.
func acceptsGzip(c *httpx.Ctx) bool {
	return strings.Contains(c.GetHeader("Accept-Encoding"), "gzip")
}
