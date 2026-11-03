// Package obsauth verifies the authentication header produced by the OBS
// Java/Python/Go SDKs.
//
// Two flavours exist and both are accepted:
//
//	Authorization: OBS <AK>:<base64(HMAC-SHA1(SK, StringToSign))>   (observ protocol, virtual hosted style)
//	Authorization: AWS <AK>:<base64(HMAC-SHA1(SK, StringToSign))>   (S3 V2 protocol, path style or CNAME)
//
// StringToSign is
//
//	HTTP-Verb + "\n" +
//	Content-MD5 + "\n" +
//	Content-Type + "\n" +
//	Date + "\n" +
//	CanonicalizedOBSHeaders +
//	CanonicalizedResource
//
// where CanonicalizedOBSHeaders is every x-obs-* (or x-amz-*) header as
// "lower-name:value\n" sorted by name, and CanonicalizedResource is the request
// path prefixed with "/<bucket>" when the bucket comes from the host name,
// followed by the signed sub-resources.
//
// The layout was verified against the real SDK (esdk-obs-java 3.24.12 and
// 3.25.5) by capturing its requests; see docs/protocol.md.
package obsauth

import (
	"crypto/hmac"
	"crypto/sha1"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
)

// ErrAnonymous is returned when the request carries no Authorization header.
var ErrAnonymous = errors.New("request is not signed")

// ErrUnsupported is returned for authentication schemes this adapter does not implement.
var ErrUnsupported = errors.New("unsupported authorization scheme")

// Result describes a verified (or accepted) request.
type Result struct {
	AccessKey string
	// Scheme is "OBS" or "AWS" (S3 signature V2).
	Scheme string
	// SignedResource is the canonicalized resource that matched.
	SignedResource string
}

// Verify checks the request signature against ak/sk.
//
// maxSkew limits the accepted difference between the Date header and now; zero
// disables the check. When ak is empty the request is accepted without
// verification and Result.AccessKey is empty.
func Verify(r *http.Request, bucket, ak, sk string, maxSkew time.Duration, now time.Time) (*Result, error) {
	if ak == "" {
		// Signature verification disabled: trust the caller.
		return &Result{}, nil
	}
	header := strings.TrimSpace(r.Header.Get("Authorization"))
	if header == "" {
		return nil, ErrAnonymous
	}
	if strings.HasPrefix(header, "AWS4-HMAC-SHA256") {
		return nil, fmt.Errorf("%w: AWS signature V4 (%s)", ErrUnsupported, header[:min(len(header), 24)])
	}

	scheme, cred, ok := strings.Cut(header, " ")
	if !ok {
		return nil, fmt.Errorf("%w: %q", ErrUnsupported, header)
	}
	var prefix string
	switch scheme {
	case "OBS":
		prefix = "x-obs-"
	case "AWS":
		prefix = "x-amz-"
	default:
		return nil, fmt.Errorf("%w: %q", ErrUnsupported, scheme)
	}

	gotAK, gotSig, ok := strings.Cut(cred, ":")
	if !ok {
		return nil, fmt.Errorf("%w: malformed credentials", ErrUnsupported)
	}
	if gotAK != ak {
		return nil, &Error{Code: "InvalidAccessKeyId", Message: "The access key ID you provided does not exist in our records.", Status: http.StatusForbidden}
	}

	date, err := requestDate(r, prefix)
	if err != nil {
		return nil, err
	}
	if maxSkew > 0 {
		if d := now.Sub(date); d > maxSkew || d < -maxSkew {
			return nil, &Error{Code: "RequestTimeTooSkewed", Message: "The difference between the request time and the current time is too large.", Status: http.StatusForbidden}
		}
	}

	candidates := canonicalResources(r, bucket, prefix)
	for _, res := range candidates {
		sts := stringToSign(r, prefix, res)
		sig := base64.StdEncoding.EncodeToString(hmacSHA1(sk, sts))
		if hmac.Equal([]byte(sig), []byte(gotSig)) {
			return &Result{AccessKey: gotAK, Scheme: scheme, SignedResource: res}, nil
		}
	}
	return nil, &Error{Code: "SignatureDoesNotMatch", Message: "The request signature we calculated does not match the signature you provided.", Status: http.StatusForbidden}
}

// Error is an OBS style error, carrying the XML error code the clients expect.
type Error struct {
	Code    string
	Message string
	Status  int
}

func (e *Error) Error() string { return e.Code + ": " + e.Message }

// IsError reports whether err is an *Error and returns it.
func IsError(err error) (*Error, bool) {
	var e *Error
	if errors.As(err, &e) {
		return e, true
	}
	return nil, false
}

func requestDate(r *http.Request, prefix string) (time.Time, error) {
	if v := r.Header.Get(strings.TrimSuffix(prefix, "-") + "-date"); v != "" {
		if t, err := http.ParseTime(v); err == nil {
			return t, nil
		}
	}
	v := r.Header.Get("Date")
	if v == "" {
		return time.Time{}, &Error{Code: "AccessDenied", Message: "A valid Date or x-obs-date header is required for all requests.", Status: http.StatusForbidden}
	}
	t, err := http.ParseTime(v)
	if err != nil {
		return time.Time{}, &Error{Code: "AccessDenied", Message: "Date header is not in RFC 822 format.", Status: http.StatusForbidden}
	}
	return t, nil
}

// stringToSign assembles the canonical string exactly like the SDK does.
func stringToSign(r *http.Request, prefix, resource string) string {
	var b strings.Builder
	b.WriteString(r.Method)
	b.WriteString("\n")
	b.WriteString(r.Header.Get("Content-MD5"))
	b.WriteString("\n")
	b.WriteString(r.Header.Get("Content-Type"))
	b.WriteString("\n")
	if d := r.Header.Get(strings.TrimSuffix(prefix, "-") + "-date"); d != "" {
		b.WriteString(d)
	} else {
		b.WriteString(r.Header.Get("Date"))
	}
	b.WriteString("\n")
	for _, h := range canonicalHeaders(r, prefix) {
		b.WriteString(h)
	}
	b.WriteString(resource)
	return b.String()
}

// canonicalHeaders renders the signed x-obs-*/x-amz-* headers.
func canonicalHeaders(r *http.Request, prefix string) []string {
	names := make([]string, 0, len(r.Header))
	for name := range r.Header {
		lower := strings.ToLower(name)
		if strings.HasPrefix(lower, prefix) {
			names = append(names, lower)
		}
	}
	sort.Strings(names)
	out := make([]string, 0, len(names))
	for _, name := range names {
		values := r.Header.Values(name)
		for i := range values {
			values[i] = strings.TrimSpace(values[i])
		}
		out = append(out, name+":"+strings.Join(values, ",")+"\n")
	}
	return out
}

// canonicalResources returns the resource spellings a client may have signed.
// The Java SDK uses the path as sent on the wire; for virtual hosted requests
// it prefixes "/<bucket>". We accept both so that clients behind a reverse
// proxy that rewrites the Host header keep working.
func canonicalResources(r *http.Request, bucket, prefix string) []string {
	path := r.URL.EscapedPath()
	if path == "" {
		path = "/"
	}
	sub := signedSubResources(r, prefix)
	joined := path
	if bucket != "" {
		joined = "/" + bucket + path
	}
	if bucket != "" && joined != path {
		return []string{joined + sub, path + sub}
	}
	return []string{joined + sub}
}

// signedSubResources renders the sub-resources that participate in the
// signature, sorted, with decoded values (that is what the SDK does).
func signedSubResources(r *http.Request, prefix string) string {
	q := r.URL.Query()
	if len(q) == 0 {
		return ""
	}
	names := make([]string, 0, len(q))
	for name := range q {
		lower := strings.ToLower(name)
		if signedSubResource[lower] || strings.HasPrefix(lower, prefix) {
			names = append(names, name)
		}
	}
	if len(names) == 0 {
		return ""
	}
	sort.Slice(names, func(i, j int) bool { return strings.ToLower(names[i]) < strings.ToLower(names[j]) })
	var b strings.Builder
	b.WriteString("?")
	for i, name := range names {
		if i > 0 {
			b.WriteString("&")
		}
		b.WriteString(name)
		if values := q[name]; len(values) > 0 && values[0] != "" {
			b.WriteString("=")
			b.WriteString(values[0])
		}
	}
	return b.String()
}

// signedSubResource mirrors Constants.ALLOWED_RESOURCE_PARAMTER_NAMES of the
// Java SDK: only these query parameters are part of the canonical resource.
var signedSubResource = map[string]bool{
	"acl": true, "backtosource": true, "policy": true, "torrent": true,
	"logging": true, "location": true, "storageinfo": true, "quota": true,
	"storagepolicy": true, "storageclass": true, "requestpayment": true,
	"versions": true, "versioning": true, "versionid": true, "uploads": true,
	"uploadid": true, "partnumber": true, "website": true, "notification": true,
	"lifecycle": true, "deletebucket": true, "delete": true, "cors": true,
	"restore": true, "tagging": true, "replication": true, "metadata": true,
	"encryption": true, "directcoldaccess": true, "inventory": true,
	"append": true, "position": true, "truncate": true, "modify": true,
	"rename": true, "length": true, "name": true, "fileinterface": true,
	"x-image-save-bucket": true, "x-image-save-object": true, "x-image-process": true,
	"obsbucketalias": true, "obsalias": true, "replication_progress": true,
	"getcontentsummary": true, "select": true, "select-type": true,
}

func hmacSHA1(secret, data string) []byte {
	m := hmac.New(sha1.New, []byte(secret))
	m.Write([]byte(data))
	return m.Sum(nil)
}

// PresignPath is a helper used by tests to build the string a client signs.
func PresignPath(bucket string, u *url.URL) string {
	path := u.EscapedPath()
	if bucket == "" {
		return path
	}
	return "/" + bucket + path
}
