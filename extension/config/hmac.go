package config

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// hmacScheme is the Authorization scheme shared by the config
// server and its clients:
//
//	Authorization: Nexus-Config-HMAC <unix-ts>:<base64-hmac-sha256>
//
// The signed bytes are <app>:<unix-ts>:<request-path>. Same
// construction as extension/peer's HMAC but over different fields
// (the body is empty for a GET; the path uniquely identifies the
// requested snapshot).
const hmacScheme = "Nexus-Config-HMAC"

// hmacMaxSkew bounds the accepted clock drift between client and
// server. A signed header is a bearer credential for its path, so
// the window also caps how long a captured header can be replayed.
const hmacMaxSkew = 30 * time.Second

// configHMACHeader builds the Authorization header value a client
// stamps on a request for app's snapshot at path.
func configHMACHeader(app, path, secret string, now time.Time) string {
	ts := strconv.FormatInt(now.Unix(), 10)
	return hmacScheme + " " + ts + ":" + configHMACSignature(app, ts, path, secret)
}

func configHMACSignature(app, ts, path, secret string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(app + ":" + ts + ":" + path))
	return base64.StdEncoding.EncodeToString(mac.Sum(nil))
}

// verifyConfigHMAC is the per-request HMAC bearer check (see
// hmacScheme for the wire format). The path is taken from the
// request, so a signature for one app/profile can't fetch another.
func verifyConfigHMAC(r *http.Request, app, secret string) error {
	if secret == "" {
		return fmt.Errorf("HMAC: no secret configured for this app")
	}
	header := r.Header.Get("Authorization")
	if header == "" {
		return fmt.Errorf("HMAC: missing Authorization header")
	}
	rest, ok := strings.CutPrefix(header, hmacScheme+" ")
	if !ok {
		return fmt.Errorf("HMAC: Authorization header is not %s", hmacScheme)
	}
	ts, sig, ok := strings.Cut(rest, ":")
	if !ok {
		return fmt.Errorf("HMAC: malformed Authorization header")
	}
	unix, err := strconv.ParseInt(ts, 10, 64)
	if err != nil {
		return fmt.Errorf("HMAC: malformed timestamp")
	}
	if d := time.Since(time.Unix(unix, 0)); d > hmacMaxSkew || d < -hmacMaxSkew {
		return fmt.Errorf("HMAC: timestamp outside the %s skew window", hmacMaxSkew)
	}
	expected := configHMACSignature(app, ts, r.URL.Path, secret)
	if !hmac.Equal([]byte(expected), []byte(sig)) {
		return fmt.Errorf("HMAC: signature mismatch")
	}
	return nil
}
