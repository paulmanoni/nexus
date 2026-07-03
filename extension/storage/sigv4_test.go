package storage

import (
	"net/http"
	"strings"
	"testing"
	"time"
)

// TestSigV4AgainstAWSVector pins signRequest to AWS's own documented
// "GET Object" example, the authoritative correctness check for the
// signing implementation.
//
// https://docs.aws.amazon.com/AmazonS3/latest/API/sig-v4-header-based-auth.html
//
//	bucket=examplebucket, key=test.txt, Range: bytes=0-9,
//	AKIAIOSFODNN7EXAMPLE / wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY,
//	us-east-1, 20130524T000000Z
//	→ Signature f0e8bdb87c964420e857bd35b5d6ed310bd44f0170aba48dd91039c6036bdb41
func TestSigV4AgainstAWSVector(t *testing.T) {
	t.Parallel()
	req, _ := http.NewRequest(http.MethodGet, "https://examplebucket.s3.amazonaws.com/test.txt", nil)
	req.Host = "examplebucket.s3.amazonaws.com"
	req.Header.Set("Range", "bytes=0-9")

	creds := sigV4Credentials{
		AccessKey: "AKIAIOSFODNN7EXAMPLE",
		SecretKey: "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY",
		Region:    "us-east-1",
	}
	when := time.Date(2013, 5, 24, 0, 0, 0, 0, time.UTC)

	signRequest(req, creds, emptyPayloadSHA, when)

	auth := req.Header.Get("Authorization")
	const wantSig = "Signature=f0e8bdb87c964420e857bd35b5d6ed310bd44f0170aba48dd91039c6036bdb41"
	if !strings.Contains(auth, wantSig) {
		t.Fatalf("signature mismatch\n got: %s\nwant substring: %s", auth, wantSig)
	}
	const wantSigned = "SignedHeaders=host;range;x-amz-content-sha256;x-amz-date"
	if !strings.Contains(auth, wantSigned) {
		t.Fatalf("signed headers mismatch\n got: %s\nwant substring: %s", auth, wantSigned)
	}
}

func TestURIEncodePath(t *testing.T) {
	t.Parallel()
	cases := map[string]string{
		"/a/b.txt":       "/a/b.txt",
		"/a/b c.txt":     "/a/b%20c.txt",
		"/photos/2024/x": "/photos/2024/x",
		"/a+b/c&d":       "/a%2Bb/c%26d",
	}
	for in, want := range cases {
		if got := uriEncodePath(in); got != want {
			t.Errorf("uriEncodePath(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestPresignURLStructure(t *testing.T) {
	t.Parallel()
	creds := sigV4Credentials{AccessKey: "AK", SecretKey: "SK", Region: "us-east-1"}
	when := time.Date(2024, 1, 2, 3, 4, 5, 0, time.UTC)
	got, err := presignURL("https://b.s3.us-east-1.amazonaws.com/k.png", "b.s3.us-east-1.amazonaws.com", creds, 15*time.Minute, when)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"X-Amz-Algorithm=AWS4-HMAC-SHA256",
		"X-Amz-Expires=900",
		"X-Amz-Signature=",
		"X-Amz-Credential=AK",
		"X-Amz-Date=20240102T030405Z",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("presigned URL missing %q\n got: %s", want, got)
		}
	}
}
