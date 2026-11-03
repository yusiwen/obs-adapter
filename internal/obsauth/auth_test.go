package obsauth

import (
	"crypto/hmac"
	"crypto/sha1"
	"encoding/base64"
	"net/http"
	"testing"
	"time"
)

const (
	testAK = "AKTEST1234567890"
	testSK = "SKtestsecretkey0123456789"
	// Fixed request date used by every case; matches the capture from the real
	// SDK so the expected string-to-sign values are the ones the SDK produced.
	testDate = "Tue, 03 Nov 2026 03:47:15 GMT"
)

var testNow = time.Date(2026, 11, 3, 3, 47, 15, 0, time.UTC)

// signWithPrefix mirrors what esdk-obs-java does: build StringToSign from the
// literal layout, HMAC-SHA1 it and put "<SCHEME> <AK>:<sig>" in Authorization.
// The expected strings are written out in full so a change in the adapter's
// construction fails loudly.
func signWithPrefix(t *testing.T, r *http.Request, scheme, expectedStringToSign string) {
	t.Helper()
	m := hmac.New(sha1.New, []byte(testSK))
	m.Write([]byte(expectedStringToSign))
	sig := base64.StdEncoding.EncodeToString(m.Sum(nil))
	r.Header.Set("Authorization", scheme+" "+testAK+":"+sig)
}

func TestVerifyObsScheme(t *testing.T) {
	// Virtual hosted style: the client signs "/<bucket><path>" with x-obs-* headers.
	r, err := http.NewRequest(http.MethodPut, "http://my-bucket.obs.test/affix%2F2026%2F11%2F03%2Ftest.xlsx", nil)
	if err != nil {
		t.Fatal(err)
	}
	r.Header.Set("Date", testDate)
	r.Header.Set("Content-Type", "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet")
	r.Header.Set("Content-Length", "5")

	want := "PUT\n" +
		"\n" +
		"application/vnd.openxmlformats-officedocument.spreadsheetml.sheet\n" +
		testDate + "\n" +
		"/my-bucket/affix%2F2026%2F11%2F03%2Ftest.xlsx"
	signWithPrefix(t, r, "OBS", want)

	res, err := Verify(r, "my-bucket", testAK, testSK, 15*time.Minute, testNow)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if res.Scheme != "OBS" || res.SignedResource != "/my-bucket/affix%2F2026%2F11%2F03%2Ftest.xlsx" {
		t.Fatalf("unexpected result: %+v", res)
	}
}

func TestVerifyV2SchemeWithMeta(t *testing.T) {
	// Path style: the SDK switches to the S3 V2 signature with x-amz-* headers.
	r, err := http.NewRequest(http.MethodPut, "http://obs.test/my-bucket/dir/obj.bin", nil)
	if err != nil {
		t.Fatal(err)
	}
	r.Header.Set("Date", testDate)
	r.Header.Set("x-amz-meta-owner", "alice")
	r.Header.Set("x-amz-acl", "private")

	want := "PUT\n" +
		"\n" +
		"\n" +
		testDate + "\n" +
		"x-amz-acl:private\n" +
		"x-amz-meta-owner:alice\n" +
		"/my-bucket/dir/obj.bin"
	signWithPrefix(t, r, "AWS", want)

	if _, err := Verify(r, "my-bucket", testAK, testSK, 15*time.Minute, testNow); err != nil {
		t.Fatalf("Verify: %v", err)
	}
}

func TestVerifySubResource(t *testing.T) {
	r, err := http.NewRequest(http.MethodGet, "http://obs.test/my-bucket?acl&max-keys=5", nil)
	if err != nil {
		t.Fatal(err)
	}
	r.Header.Set("Date", testDate)

	// Only ACL is a signed sub-resource; max-keys is not part of the signature.
	want := "GET\n\n\n" + testDate + "\n/my-bucket?acl"
	signWithPrefix(t, r, "OBS", want)

	if _, err := Verify(r, "my-bucket", testAK, testSK, 15*time.Minute, testNow); err != nil {
		t.Fatalf("Verify: %v", err)
	}
}

func TestVerifyRejections(t *testing.T) {
	newReq := func() *http.Request {
		r, _ := http.NewRequest(http.MethodGet, "http://obs.test/my-bucket/obj", nil)
		r.Header.Set("Date", testDate)
		return r
	}

	t.Run("wrong secret", func(t *testing.T) {
		r := newReq()
		signWithPrefix(t, r, "OBS", "GET\n\n\n"+testDate+"\n/my-bucket/obj")
		_, err := Verify(r, "my-bucket", testAK, "different-secret", 15*time.Minute, testNow)
		e, ok := IsError(err)
		if !ok || e.Code != "SignatureDoesNotMatch" || e.Status != http.StatusForbidden {
			t.Fatalf("got %v, want SignatureDoesNotMatch", err)
		}
	})

	t.Run("wrong access key", func(t *testing.T) {
		r := newReq()
		signWithPrefix(t, r, "OBS", "GET\n\n\n"+testDate+"\n/my-bucket/obj")
		_, err := Verify(r, "my-bucket", "AKOTHER", testSK, 15*time.Minute, testNow)
		e, ok := IsError(err)
		if !ok || e.Code != "InvalidAccessKeyId" {
			t.Fatalf("got %v, want InvalidAccessKeyId", err)
		}
	})

	t.Run("clock skew", func(t *testing.T) {
		r := newReq()
		signWithPrefix(t, r, "OBS", "GET\n\n\n"+testDate+"\n/my-bucket/obj")
		_, err := Verify(r, "my-bucket", testAK, testSK, 5*time.Minute, testNow.Add(time.Hour))
		e, ok := IsError(err)
		if !ok || e.Code != "RequestTimeTooSkewed" {
			t.Fatalf("got %v, want RequestTimeTooSkewed", err)
		}
	})

	t.Run("anonymous", func(t *testing.T) {
		if _, err := Verify(newReq(), "my-bucket", testAK, testSK, 0, testNow); err != ErrAnonymous {
			t.Fatalf("got %v, want ErrAnonymous", err)
		}
	})

	t.Run("sigv4 is not supported", func(t *testing.T) {
		r := newReq()
		r.Header.Set("Authorization", "AWS4-HMAC-SHA256 Credential=AK/x, SignedHeaders=host, Signature=y")
		if _, err := Verify(r, "my-bucket", testAK, testSK, 0, testNow); err == nil {
			t.Fatal("expected an error")
		}
	})

	t.Run("verification disabled accepts anything", func(t *testing.T) {
		if _, err := Verify(newReq(), "my-bucket", "", "", 0, testNow); err != nil {
			t.Fatalf("got %v, want nil", err)
		}
	})
}
