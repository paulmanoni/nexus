package storage

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

// AWS Signature Version 4 for S3, implemented against only the standard
// library so no AWS SDK is linked. Reference:
// https://docs.aws.amazon.com/AmazonS3/latest/API/sig-v4-authenticating-requests.html
//
// The logic is exercised in sigv4_test.go against AWS's own documented
// "GET Object" example vector, which pins the derived signature.

const (
	sigV4Algorithm = "AWS4-HMAC-SHA256"
	sigV4Service   = "s3"
	// unsignedPayload lets us sign a streaming body over HTTPS without
	// hashing it first (S3 accepts this sentinel as x-amz-content-sha256).
	unsignedPayload = "UNSIGNED-PAYLOAD"
	emptyPayloadSHA = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
)

type sigV4Credentials struct {
	AccessKey    string
	SecretKey    string
	SessionToken string // optional (STS)
	Region       string
}

func hmacSHA256(key []byte, data string) []byte {
	h := hmac.New(sha256.New, key)
	h.Write([]byte(data))
	return h.Sum(nil)
}

func sha256Hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// signingKey derives the SigV4 signing key for a date/region/service.
func signingKey(secret, dateStamp, region, service string) []byte {
	kDate := hmacSHA256([]byte("AWS4"+secret), dateStamp)
	kRegion := hmacSHA256(kDate, region)
	kService := hmacSHA256(kRegion, service)
	return hmacSHA256(kService, "aws4_request")
}

// uriEncodePath encodes an S3 object key for the canonical URI. Every
// segment is URL-encoded, but "/" separators are preserved (S3 treats the
// key path hierarchically). AWS's rules: unreserved chars pass through,
// everything else is %XX (uppercase hex), and "/" is NOT encoded here.
func uriEncodePath(p string) string {
	segs := strings.Split(p, "/")
	for i, s := range segs {
		segs[i] = awsURIEncode(s, false)
	}
	return strings.Join(segs, "/")
}

// awsURIEncode implements AWS's URI encoding (RFC 3986 unreserved set kept
// literal). encodeSlash controls whether "/" is percent-encoded (true for
// query values, false for path segments).
func awsURIEncode(s string, encodeSlash bool) string {
	const unreserved = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_.~"
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case strings.IndexByte(unreserved, c) >= 0:
			b.WriteByte(c)
		case c == '/' && !encodeSlash:
			b.WriteByte('/')
		default:
			b.WriteByte('%')
			b.WriteString(strings.ToUpper(hex.EncodeToString([]byte{c})))
		}
	}
	return b.String()
}

// signRequest adds the SigV4 Authorization (and x-amz-*) headers to req in
// place. payloadHash is the hex sha256 of the body, or unsignedPayload.
// now is the signing time (pass an explicit value so tests are stable).
func signRequest(req *http.Request, creds sigV4Credentials, payloadHash string, now time.Time) {
	amzDate := now.UTC().Format("20060102T150405Z")
	dateStamp := now.UTC().Format("20060102")

	req.Header.Set("X-Amz-Date", amzDate)
	req.Header.Set("X-Amz-Content-Sha256", payloadHash)
	if creds.SessionToken != "" {
		req.Header.Set("X-Amz-Security-Token", creds.SessionToken)
	}
	if req.Host == "" {
		req.Host = req.URL.Host
	}

	canonicalHeaders, signedHeaders := canonicalHeaders(req)
	canonicalReq := strings.Join([]string{
		req.Method,
		uriEncodePath(req.URL.Path),
		canonicalQuery(req.URL.Query()),
		canonicalHeaders,
		signedHeaders,
		payloadHash,
	}, "\n")

	scope := strings.Join([]string{dateStamp, creds.Region, sigV4Service, "aws4_request"}, "/")
	stringToSign := strings.Join([]string{
		sigV4Algorithm,
		amzDate,
		scope,
		sha256Hex(canonicalReq),
	}, "\n")

	sig := hex.EncodeToString(hmacSHA256(signingKey(creds.SecretKey, dateStamp, creds.Region, sigV4Service), stringToSign))
	auth := sigV4Algorithm +
		" Credential=" + creds.AccessKey + "/" + scope +
		", SignedHeaders=" + signedHeaders +
		", Signature=" + sig
	req.Header.Set("Authorization", auth)
}

// unsignedHeaders are the volatile headers net/http populates during
// transport (after we sign) — excluding them keeps the signature valid.
var unsignedHeaders = map[string]bool{
	"authorization":   true,
	"user-agent":      true,
	"content-length":  true,
	"accept-encoding": true,
}

// canonicalHeaders builds the sorted canonical header block + the
// semicolon-joined signed-header list. host is always included; every
// other caller-set header is signed except the volatile ones above. This
// matches AWS's canonicalization (verified against the documented vector).
func canonicalHeaders(req *http.Request) (string, string) {
	headers := map[string]string{"host": req.Host}
	for k, v := range req.Header {
		lk := strings.ToLower(k)
		if unsignedHeaders[lk] {
			continue
		}
		headers[lk] = strings.TrimSpace(strings.Join(v, ","))
	}
	keys := make([]string, 0, len(headers))
	for k := range headers {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for _, k := range keys {
		b.WriteString(k)
		b.WriteByte(':')
		b.WriteString(headers[k])
		b.WriteByte('\n')
	}
	return b.String(), strings.Join(keys, ";")
}

// canonicalQuery renders query params in the canonical (sorted, encoded)
// form SigV4 requires.
func canonicalQuery(q url.Values) string {
	keys := make([]string, 0, len(q))
	for k := range q {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var parts []string
	for _, k := range keys {
		vals := append([]string(nil), q[k]...)
		sort.Strings(vals)
		for _, v := range vals {
			parts = append(parts, awsURIEncode(k, true)+"="+awsURIEncode(v, true))
		}
	}
	return strings.Join(parts, "&")
}

// presignURL returns a presigned GET URL for req.URL valid for expiry,
// with the signature carried in query params (SigV4 query signing).
func presignURL(rawURL string, host string, creds sigV4Credentials, expiry time.Duration, now time.Time) (string, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return "", err
	}
	amzDate := now.UTC().Format("20060102T150405Z")
	dateStamp := now.UTC().Format("20060102")
	scope := strings.Join([]string{dateStamp, creds.Region, sigV4Service, "aws4_request"}, "/")

	q := u.Query()
	q.Set("X-Amz-Algorithm", sigV4Algorithm)
	q.Set("X-Amz-Credential", creds.AccessKey+"/"+scope)
	q.Set("X-Amz-Date", amzDate)
	q.Set("X-Amz-Expires", strconv.Itoa(int(expiry.Seconds())))
	q.Set("X-Amz-SignedHeaders", "host")
	if creds.SessionToken != "" {
		q.Set("X-Amz-Security-Token", creds.SessionToken)
	}

	canonicalReq := strings.Join([]string{
		http.MethodGet,
		uriEncodePath(u.Path),
		canonicalQuery(q),
		"host:" + host + "\n",
		"host",
		unsignedPayload,
	}, "\n")
	stringToSign := strings.Join([]string{sigV4Algorithm, amzDate, scope, sha256Hex(canonicalReq)}, "\n")
	sig := hex.EncodeToString(hmacSHA256(signingKey(creds.SecretKey, dateStamp, creds.Region, sigV4Service), stringToSign))
	q.Set("X-Amz-Signature", sig)
	u.RawQuery = q.Encode()
	return u.String(), nil
}
