package s3sig

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestEncodePath(t *testing.T) {
	cases := []struct{ in, want string }{
		{"/test/key.txt", "/test/key.txt"},
		{"/test/a b/c.txt", "/test/a%20b/c.txt"},
		{"/test/测试/文件 (1).xlsx", "/test/%E6%B5%8B%E8%AF%95/%E6%96%87%E4%BB%B6%20%281%29.xlsx"},
		{"/test/100%.txt", "/test/100%25.txt"},
		{"/test/a+b", "/test/a%2Bb"},
		{"/", "/"},
		{"", "/"},
	}
	for _, c := range cases {
		if got := EncodePath(c.in); got != c.want {
			t.Errorf("EncodePath(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestCanonicalQueryString(t *testing.T) {
	q := url.Values{"prefix": {"a/b"}, "max-keys": {"5"}, "uploads": {""}}
	got := CanonicalQueryString(q)
	want := "max-keys=5&prefix=a%2Fb&uploads="
	if got != want {
		t.Fatalf("CanonicalQueryString = %q, want %q", got, want)
	}
}

// The expected signatures below were produced by an independent implementation
// (Python hmac) of the AWS SigV4 algorithm, so they pin the canonicalisation
// rules rather than mirroring this package's code.
func TestSignGolden(t *testing.T) {
	creds := Credentials{AccessKey: "AKTEST", SecretKey: "SKtest", Region: "us-east-1"}
	at := time.Date(2026, 11, 3, 4, 0, 0, 0, time.UTC)

	t.Run("get unsigned payload", func(t *testing.T) {
		req, err := http.NewRequest(http.MethodGet, "http://127.0.0.1:9000/test/key.txt", nil)
		if err != nil {
			t.Fatal(err)
		}
		Sign(req, creds, UnsignedPayload, at)

		want := "AWS4-HMAC-SHA256 Credential=AKTEST/20261103/us-east-1/s3/aws4_request, " +
			"SignedHeaders=host;x-amz-content-sha256;x-amz-date, " +
			"Signature=5c2a419e1eaf4661b746bd13b3d4cb448694691c961510af8cc644a2e711c0c7"
		if got := req.Header.Get("Authorization"); got != want {
			t.Fatalf("Authorization =\n%q\nwant\n%q", got, want)
		}
	})

	t.Run("put with content type and query", func(t *testing.T) {
		req, err := http.NewRequest(http.MethodPut, "http://127.0.0.1:9000/test/a%20b/c.txt?prefix=a/b&max-keys=5", nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/xml")
		Sign(req, creds, EmptyPayload, at)

		want := "AWS4-HMAC-SHA256 Credential=AKTEST/20261103/us-east-1/s3/aws4_request, " +
			"SignedHeaders=content-type;host;x-amz-content-sha256;x-amz-date, " +
			"Signature=60f29e3e5108ad8b5feef604ccd48cd5be194f6dba13f68a8b0cd4983960bb47"
		if got := req.Header.Get("Authorization"); got != want {
			t.Fatalf("Authorization =\n%q\nwant\n%q", got, want)
		}
		if got := req.URL.RawQuery; got != "max-keys=5&prefix=a%2Fb" {
			t.Fatalf("RawQuery = %q, want canonical form", got)
		}
	})

	t.Run("region defaults", func(t *testing.T) {
		req, _ := http.NewRequest(http.MethodGet, "http://127.0.0.1:9000/test/key.txt", nil)
		Sign(req, Credentials{AccessKey: "AK", SecretKey: "SK"}, EmptyPayload, at)
		if !strings.Contains(req.Header.Get("Authorization"), "/us-east-1/s3/aws4_request") {
			t.Fatalf("default region missing: %s", req.Header.Get("Authorization"))
		}
	})
}
