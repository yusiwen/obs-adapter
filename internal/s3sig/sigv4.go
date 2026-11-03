// Package s3sig implements AWS Signature Version 4 request signing for the
// upstream S3 (MinIO) endpoint. Only the S3 service is supported.
//
// The canonicalisation rules follow the MinIO server implementation: the
// canonical URI is produced by encoding the *decoded* request path with the
// unreserved set plus '/' (MinIO's s3utils.EncodePath), so callers must set
// URL.Path to the decoded object path and let this package encode it.
package s3sig

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
)

// Service is the SigV4 service name for S3 compatible storage.
const Service = "s3"

const (
	// UnsignedPayload lets the body stream through without hashing it first.
	UnsignedPayload = "UNSIGNED-PAYLOAD"
	// EmptyPayload is the SHA-256 of an empty body.
	EmptyPayload = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
)

// Credentials are the upstream (MinIO) credentials.
type Credentials struct {
	AccessKey string
	SecretKey string
	Region    string
}

// Sign adds the SigV4 headers to req.
//
// payloadHash must be EmptyPayload, UnsignedPayload or the hex SHA-256 of the
// body. Sign replaces the query string with its canonical form so that what is
// sent on the wire is exactly what was signed.
func Sign(req *http.Request, creds Credentials, payloadHash string, now time.Time) {
	now = now.UTC()
	amzDate := now.Format("20060102T150405Z")
	shortDate := now.Format("20060102")

	req.Header.Set("x-amz-date", amzDate)
	req.Header.Set("x-amz-content-sha256", payloadHash)

	canonicalURI := EncodePath(req.URL.Path)
	canonicalQuery := CanonicalQueryString(req.URL.Query())
	req.URL.RawQuery = canonicalQuery

	names, values := signedHeaders(req)
	signed := strings.Join(names, ";")

	var ch strings.Builder
	for _, n := range names {
		ch.WriteString(n)
		ch.WriteString(":")
		ch.WriteString(values[n])
		ch.WriteString("\n")
	}

	canonicalRequest := strings.Join([]string{
		req.Method,
		canonicalURI,
		canonicalQuery,
		ch.String(),
		signed,
		payloadHash,
	}, "\n")

	scope := shortDate + "/" + region(creds) + "/" + Service + "/aws4_request"
	sum := sha256.Sum256([]byte(canonicalRequest))
	stringToSign := strings.Join([]string{
		"AWS4-HMAC-SHA256",
		amzDate,
		scope,
		hex.EncodeToString(sum[:]),
	}, "\n")

	key := signingKey(creds.SecretKey, shortDate, region(creds))
	signature := hex.EncodeToString(hmacSHA256(key, stringToSign))

	req.Header.Set("Authorization", "AWS4-HMAC-SHA256 Credential="+creds.AccessKey+"/"+scope+
		", SignedHeaders="+signed+", Signature="+signature)
}

func region(creds Credentials) string {
	if creds.Region == "" {
		return "us-east-1"
	}
	return creds.Region
}

// signedHeaders returns the sorted header names that participate in the
// signature together with their canonical values.
func signedHeaders(req *http.Request) ([]string, map[string]string) {
	values := map[string]string{}
	values["host"] = req.Host
	if values["host"] == "" {
		values["host"] = req.URL.Host
	}
	for name := range req.Header {
		lower := strings.ToLower(name)
		switch {
		case lower == "x-amz-content-sha256", lower == "x-amz-date",
			lower == "content-type", lower == "content-md5",
			strings.HasPrefix(lower, "x-amz-"):
			v := strings.Join(req.Header.Values(name), ",")
			values[lower] = strings.TrimSpace(v)
		}
	}
	names := make([]string, 0, len(values))
	for name := range values {
		names = append(names, name)
	}
	sort.Strings(names)
	return names, values
}

// CanonicalQueryString renders a query string the way SigV4 expects it:
// parameters sorted by name, each name and value percent encoded.
func CanonicalQueryString(q url.Values) string {
	if len(q) == 0 {
		return ""
	}
	names := make([]string, 0, len(q))
	for name := range q {
		names = append(names, name)
	}
	sort.Strings(names)
	var b strings.Builder
	first := true
	sep := func() {
		if !first {
			b.WriteString("&")
		}
		first = false
	}
	for _, name := range names {
		vs := append([]string(nil), q[name]...)
		sort.Strings(vs)
		if len(vs) == 0 {
			sep()
			b.WriteString(Encode(name))
			b.WriteString("=")
			continue
		}
		for _, v := range vs {
			sep()
			b.WriteString(Encode(name))
			b.WriteString("=")
			b.WriteString(Encode(v))
		}
	}
	return b.String()
}

// EncodePath percent-encodes a path the way S3 expects: the unreserved
// characters and '/' are kept literally, everything else becomes %XX.
func EncodePath(path string) string {
	if path == "" {
		return "/"
	}
	var b strings.Builder
	for i := 0; i < len(path); i++ {
		c := path[i]
		switch {
		case c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z', c >= '0' && c <= '9',
			c == '-', c == '_', c == '.', c == '~', c == '/':
			b.WriteByte(c)
		default:
			b.WriteString(percentEncode(c))
		}
	}
	return b.String()
}

// Encode percent-encodes a query component.
func Encode(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z', c >= '0' && c <= '9',
			c == '-', c == '_', c == '.', c == '~':
			b.WriteByte(c)
		default:
			b.WriteString(percentEncode(c))
		}
	}
	return b.String()
}

const hexDigits = "0123456789ABCDEF"

func percentEncode(c byte) string {
	return string([]byte{'%', hexDigits[c>>4], hexDigits[c&0x0f]})
}

func signingKey(secret, shortDate, region string) []byte {
	kDate := hmacSHA256([]byte("AWS4"+secret), shortDate)
	kRegion := hmacSHA256(kDate, region)
	kService := hmacSHA256(kRegion, Service)
	return hmacSHA256(kService, "aws4_request")
}

func hmacSHA256(key []byte, data string) []byte {
	m := hmac.New(sha256.New, key)
	m.Write([]byte(data))
	return m.Sum(nil)
}
