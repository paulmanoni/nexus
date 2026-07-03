package storage

import (
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"path"
	"strconv"
	"strings"
	"time"
)

// S3Disk talks to any S3-compatible object store (AWS S3, MinIO,
// Cloudflare R2, DigitalOcean Spaces) directly over HTTPS with SigV4
// signing — no AWS SDK is linked. Construct it via Bind/Config or
// NewS3Disk.
type S3Disk struct {
	Bucket string
	Region string

	// Endpoint overrides the AWS host for S3-compatible stores, e.g.
	// "https://minio.internal:9000" or "https://<acct>.r2.cloudflarestorage.com".
	// Empty targets AWS S3 (virtual-hosted style).
	Endpoint string

	// PathStyle forces path-style URLs (host/bucket/key) instead of
	// virtual-hosted (bucket.host/key). Required by most non-AWS stores;
	// auto-implied when Endpoint is set.
	PathStyle bool

	AccessKey    string
	SecretKey    string
	SessionToken string

	// PublicBaseURL, when set, is what URL() returns (a CDN or public
	// bucket base). Empty → URL errors and callers use SignedURL.
	PublicBaseURL string

	// HTTPClient defaults to a client with a sane timeout when nil.
	HTTPClient *http.Client

	// now is injected in tests for deterministic signing; nil → time.Now.
	now func() time.Time
}

// NewS3Disk builds an S3Disk for the given bucket/region/credentials.
func NewS3Disk(bucket, region, accessKey, secretKey string) *S3Disk {
	return &S3Disk{Bucket: bucket, Region: region, AccessKey: accessKey, SecretKey: secretKey}
}

func (s *S3Disk) client() *http.Client {
	if s.HTTPClient != nil {
		return s.HTTPClient
	}
	return &http.Client{Timeout: 30 * time.Second}
}

func (s *S3Disk) clock() time.Time {
	if s.now != nil {
		return s.now()
	}
	return time.Now()
}

func (s *S3Disk) creds() sigV4Credentials {
	return sigV4Credentials{
		AccessKey:    s.AccessKey,
		SecretKey:    s.SecretKey,
		SessionToken: s.SessionToken,
		Region:       s.Region,
	}
}

// endpoint returns the base scheme://host and whether path-style is used.
func (s *S3Disk) baseHost() (scheme, host string, pathStyle bool) {
	if s.Endpoint != "" {
		e := s.Endpoint
		scheme = "https"
		if strings.HasPrefix(e, "http://") {
			scheme = "http"
			e = strings.TrimPrefix(e, "http://")
		} else {
			e = strings.TrimPrefix(e, "https://")
		}
		return scheme, strings.TrimRight(e, "/"), true // custom endpoints are path-style
	}
	if s.PathStyle {
		return "https", "s3." + s.Region + ".amazonaws.com", true
	}
	return "https", s.Bucket + ".s3." + s.Region + ".amazonaws.com", false
}

// objectURL builds the full request URL + the Host header value for a key.
func (s *S3Disk) objectURL(key string) (fullURL, host string) {
	scheme, h, pathStyle := s.baseHost()
	k := strings.TrimPrefix(key, "/")
	if pathStyle {
		return scheme + "://" + h + "/" + s.Bucket + "/" + k, h
	}
	return scheme + "://" + h + "/" + k, h
}

func (s *S3Disk) do(req *http.Request, payloadHash string) (*http.Response, error) {
	req.Header.Set("Host", req.Host)
	signRequest(req, s.creds(), payloadHash, s.clock())
	return s.client().Do(req)
}

func (s *S3Disk) Put(ctx context.Context, key string, r io.Reader, opts ...PutOption) error {
	o := applyPutOptions(opts)
	fullURL, host := s.objectURL(key)

	var body io.Reader = r
	size := o.Size
	if size < 0 {
		// Unknown length: buffer to satisfy S3's Content-Length requirement.
		buf, err := io.ReadAll(r)
		if err != nil {
			return err
		}
		body = bytes.NewReader(buf)
		size = int64(len(buf))
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPut, fullURL, body)
	if err != nil {
		return err
	}
	req.Host = host
	req.ContentLength = size
	ct := o.ContentType
	if ct == "" {
		ct = mime.TypeByExtension(path.Ext(key))
	}
	if ct != "" {
		req.Header.Set("Content-Type", ct)
	}
	if o.Public {
		req.Header.Set("X-Amz-Acl", "public-read")
	}

	resp, err := s.do(req, unsignedPayload)
	if err != nil {
		return err
	}
	defer drain(resp)
	if resp.StatusCode/100 != 2 {
		return s3Error("PUT", key, resp)
	}
	return nil
}

func (s *S3Disk) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	fullURL, host := s.objectURL(key)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, fullURL, nil)
	if err != nil {
		return nil, err
	}
	req.Host = host
	resp, err := s.do(req, emptyPayloadSHA)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode == http.StatusNotFound {
		drain(resp)
		return nil, ErrNotExist
	}
	if resp.StatusCode/100 != 2 {
		defer drain(resp)
		return nil, s3Error("GET", key, resp)
	}
	return resp.Body, nil
}

func (s *S3Disk) head(ctx context.Context, key string) (*http.Response, error) {
	fullURL, host := s.objectURL(key)
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, fullURL, nil)
	if err != nil {
		return nil, err
	}
	req.Host = host
	return s.do(req, emptyPayloadSHA)
}

func (s *S3Disk) Exists(ctx context.Context, key string) (bool, error) {
	resp, err := s.head(ctx, key)
	if err != nil {
		return false, err
	}
	defer drain(resp)
	if resp.StatusCode == http.StatusNotFound {
		return false, nil
	}
	if resp.StatusCode/100 != 2 {
		return false, s3Error("HEAD", key, resp)
	}
	return true, nil
}

func (s *S3Disk) Stat(ctx context.Context, key string) (Object, error) {
	resp, err := s.head(ctx, key)
	if err != nil {
		return Object{}, err
	}
	defer drain(resp)
	if resp.StatusCode == http.StatusNotFound {
		return Object{}, ErrNotExist
	}
	if resp.StatusCode/100 != 2 {
		return Object{}, s3Error("HEAD", key, resp)
	}
	obj := Object{
		Key:         strings.TrimPrefix(key, "/"),
		ContentType: resp.Header.Get("Content-Type"),
		ETag:        strings.Trim(resp.Header.Get("ETag"), `"`),
	}
	if cl := resp.Header.Get("Content-Length"); cl != "" {
		obj.Size, _ = strconv.ParseInt(cl, 10, 64)
	}
	if lm := resp.Header.Get("Last-Modified"); lm != "" {
		if t, perr := http.ParseTime(lm); perr == nil {
			obj.LastModified = t
		}
	}
	return obj, nil
}

func (s *S3Disk) Delete(ctx context.Context, key string) error {
	fullURL, host := s.objectURL(key)
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, fullURL, nil)
	if err != nil {
		return err
	}
	req.Host = host
	resp, err := s.do(req, emptyPayloadSHA)
	if err != nil {
		return err
	}
	defer drain(resp)
	// S3 DELETE is idempotent — a missing key still returns 204. Report
	// ErrNotExist only when the store explicitly says 404.
	if resp.StatusCode == http.StatusNotFound {
		return ErrNotExist
	}
	if resp.StatusCode/100 != 2 {
		return s3Error("DELETE", key, resp)
	}
	return nil
}

// listBucketResult mirrors the S3 ListObjectsV2 XML response (the subset
// we consume).
type listBucketResult struct {
	Contents []struct {
		Key          string    `xml:"Key"`
		Size         int64     `xml:"Size"`
		ETag         string    `xml:"ETag"`
		LastModified time.Time `xml:"LastModified"`
	} `xml:"Contents"`
	IsTruncated           bool   `xml:"IsTruncated"`
	NextContinuationToken string `xml:"NextContinuationToken"`
}

func (s *S3Disk) List(ctx context.Context, prefix string) ([]Object, error) {
	scheme, host, pathStyle := s.baseHost()
	base := scheme + "://" + host + "/"
	if pathStyle {
		base += s.Bucket + "/"
	}

	var out []Object
	token := ""
	for {
		u := base + "?list-type=2&prefix=" + awsURIEncode(strings.TrimPrefix(prefix, "/"), true)
		if token != "" {
			u += "&continuation-token=" + awsURIEncode(token, true)
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
		if err != nil {
			return nil, err
		}
		req.Host = host
		resp, err := s.do(req, emptyPayloadSHA)
		if err != nil {
			return nil, err
		}
		if resp.StatusCode/100 != 2 {
			defer drain(resp)
			return nil, s3Error("LIST", prefix, resp)
		}
		var res listBucketResult
		dec := xml.NewDecoder(resp.Body)
		derr := dec.Decode(&res)
		drain(resp)
		if derr != nil {
			return nil, derr
		}
		for _, c := range res.Contents {
			out = append(out, Object{
				Key:          c.Key,
				Size:         c.Size,
				ETag:         strings.Trim(c.ETag, `"`),
				LastModified: c.LastModified,
			})
		}
		if !res.IsTruncated || res.NextContinuationToken == "" {
			break
		}
		token = res.NextContinuationToken
	}
	return out, nil
}

func (s *S3Disk) URL(key string) (string, error) {
	if s.PublicBaseURL != "" {
		return strings.TrimRight(s.PublicBaseURL, "/") + "/" + strings.TrimPrefix(key, "/"), nil
	}
	return "", errors.New("storage: S3Disk has no PublicBaseURL configured (use SignedURL for private objects)")
}

func (s *S3Disk) SignedURL(ctx context.Context, key string, expiry time.Duration) (string, error) {
	fullURL, host := s.objectURL(key)
	return presignURL(fullURL, host, s.creds(), expiry, s.clock())
}

func (s *S3Disk) Driver() string { return "s3" }
func (s *S3Disk) DiskDetails() map[string]any {
	_, host, _ := s.baseHost()
	return map[string]any{"driver": "s3", "bucket": s.Bucket, "region": s.Region, "host": host}
}

// Ping is a no-op health probe: an unauthenticated bucket check would
// leak credentials-free requests and a real HEAD costs a round-trip on
// every dashboard snapshot. Storage failures surface on the operation.
func (s *S3Disk) Ping(ctx context.Context) error { return nil }

// drain reads and closes a response body so the connection can be reused.
func drain(resp *http.Response) {
	if resp != nil && resp.Body != nil {
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}
}

// s3Error builds an error from a non-2xx S3 response, including the XML
// error body when present.
func s3Error(op, key string, resp *http.Response) error {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	msg := strings.TrimSpace(string(body))
	if msg == "" {
		msg = resp.Status
	}
	return fmt.Errorf("storage: S3 %s %q failed (%d): %s", op, key, resp.StatusCode, msg)
}
