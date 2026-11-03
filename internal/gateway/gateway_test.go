package gateway

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/yusiwen/obs-adapter/internal/config"
)

func testGateway(t *testing.T, upstream string, ak, sk string) *Gateway {
	t.Helper()
	cfg := &config.Config{
		Listen:         "127.0.0.1:0",
		BaseDomain:     "obs.test",
		MinioEndpoint:  upstream,
		MinioAccessKey: "minio-ak",
		MinioSecretKey: "minio-sk",
		MinioRegion:    "us-east-1",
		ObsAccessKey:   ak,
		ObsSecretKey:   sk,
		BucketAliases:  map[string]string{"legacy-bucket": "test"},
	}
	g, err := New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	return g
}

func TestRoute(t *testing.T) {
	g := testGateway(t, "http://127.0.0.1:9000", "", "")

	cases := []struct {
		host, path string
		want       route
	}{
		{"my-bucket.obs.test", "/dir/obj.txt", route{bucket: "my-bucket", key: "dir/obj.txt", vhost: true}},
		{"my-bucket.obs.test:8080", "/", route{bucket: "my-bucket", key: "", vhost: true}},
		{"obs.test", "/my-bucket/dir/obj.txt", route{bucket: "my-bucket", key: "dir/obj.txt"}},
		{"127.0.0.1:18081", "/my-bucket", route{bucket: "my-bucket", key: ""}},
		{"obs.test", "/", route{}},
	}
	for _, c := range cases {
		r := httptest.NewRequest(http.MethodGet, "http://example.com"+c.path, nil)
		r.Host = c.host
		if got := g.route(r); got != c.want {
			t.Errorf("route(%s%s) = %+v, want %+v", c.host, c.path, got, c.want)
		}
	}
}

func TestRequestHeaderTranslation(t *testing.T) {
	in := http.Header{}
	in.Set("Authorization", "OBS AK:sig")
	in.Set("Content-Type", "application/xml")
	in.Set("x-obs-meta-owner", "alice")
	in.Set("x-obs-acl", "public-read")
	in.Set("x-obs-api", "3.0")
	in.Set("Range", "bytes=0-9")
	in.Set("User-Agent", "obs-sdk-java/3.25.5")

	out := requestHeader(in)
	for _, name := range []string{"Authorization", "X-Obs-Api", "User-Agent"} {
		if _, ok := out[name]; ok {
			t.Errorf("%s must not be forwarded", name)
		}
	}
	if got := out.Get("x-amz-meta-owner"); got != "alice" {
		t.Errorf("metadata header = %q", got)
	}
	if got := out.Get("x-amz-acl"); got != "public-read" {
		t.Errorf("acl header = %q", got)
	}
	if got := out.Get("Range"); got != "bytes=0-9" {
		t.Errorf("range header = %q", got)
	}
	if got := out.Get("Content-Type"); got != "application/xml" {
		t.Errorf("content type = %q", got)
	}
}

func TestResponseHeaderTranslation(t *testing.T) {
	src := http.Header{}
	src.Set("ETag", `"abc"`)
	src.Set("Content-Type", "application/octet-stream")
	src.Set("x-amz-request-id", "upstream-id")
	src.Set("x-amz-version-id", "v1")
	src.Set("x-amz-meta-owner", "alice")
	src.Set("x-amz-storage-class", "STANDARD")
	src.Set("x-amz-something-else", "dropped")
	src.Set("Server", "MinIO")

	dst := http.Header{}
	responseHeader(dst, src, "req-1")

	if got := dst.Get("ETag"); got != `"abc"` {
		t.Errorf("etag = %q", got)
	}
	if got := dst.Get("x-obs-request-id"); got != "req-1" {
		t.Errorf("request id = %q", got)
	}
	if got := dst.Get("x-obs-version-id"); got != "v1" {
		t.Errorf("version id = %q", got)
	}
	if got := dst.Get("x-obs-meta-owner"); got != "alice" {
		t.Errorf("metadata = %q", got)
	}
	if got := dst.Get("x-obs-storage-class"); got != "STANDARD" {
		t.Errorf("storage class = %q", got)
	}
	if got := dst.Get("x-obs-api"); got != "3.0" {
		t.Errorf("api version header = %q", got)
	}
	if got := dst.Get("x-amz-something-else"); got != "" {
		t.Errorf("unknown x-amz header leaked: %q", got)
	}
	if got := dst.Get("Server"); got != "" {
		t.Errorf("server header leaked: %q", got)
	}
}

func TestApiVersionNegotiation(t *testing.T) {
	g := testGateway(t, "http://127.0.0.1:9000", "", "")

	r := httptest.NewRequest(http.MethodHead, "http://my-bucket.obs.test/?apiversion", nil)
	r.Host = "my-bucket.obs.test"
	w := httptest.NewRecorder()
	g.ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	if got := w.Header().Get("x-obs-api"); got != "3.0" {
		t.Fatalf("x-obs-api = %q", got)
	}
}

// TestProxyTranslation drives a full request through the adapter against a fake
// upstream and asserts what MinIO would actually receive.
func TestProxyTranslation(t *testing.T) {
	type captured struct {
		method, path, query, auth, contentSha, bucketHeader string
		meta, acl, body                                     string
		host                                                string
	}
	got := &captured{}

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got.method = r.Method
		got.path = r.URL.EscapedPath()
		got.query = r.URL.RawQuery
		got.auth = r.Header.Get("Authorization")
		got.contentSha = r.Header.Get("x-amz-content-sha256")
		got.meta = r.Header.Get("x-amz-meta-owner")
		got.acl = r.Header.Get("x-amz-acl")
		got.host = r.Host
		body, _ := io.ReadAll(r.Body)
		got.body = string(body)

		w.Header().Set("ETag", `"e1"`)
		w.Header().Set("x-amz-request-id", "up1")
		w.Header().Set("x-amz-meta-owner", "alice")
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	g := testGateway(t, upstream.URL, "", "")

	r := httptest.NewRequest(http.MethodPut, "http://legacy-bucket.obs.test/dir%2Ffile%20name.txt?partNumber=1",
		strings.NewReader("payload"))
	r.Host = "legacy-bucket.obs.test"
	r.Header.Set("Content-Type", "text/plain")
	r.Header.Set("x-obs-meta-owner", "alice")
	r.Header.Set("x-obs-acl", "private")

	w := httptest.NewRecorder()
	g.ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", w.Code, w.Body.String())
	}
	if got.method != http.MethodPut {
		t.Errorf("method = %s", got.method)
	}
	// Bucket alias applied, key decoded and re-encoded.
	if got.path != "/test/dir/file%20name.txt" {
		t.Errorf("upstream path = %q", got.path)
	}
	if got.query != "partNumber=1" {
		t.Errorf("upstream query = %q", got.query)
	}
	if !strings.HasPrefix(got.auth, "AWS4-HMAC-SHA256 Credential=minio-ak/") {
		t.Errorf("upstream not signed with V4: %q", got.auth)
	}
	if got.meta != "alice" || got.acl != "private" {
		t.Errorf("headers not translated: meta=%q acl=%q", got.meta, got.acl)
	}
	if got.body != "payload" {
		t.Errorf("body = %q", got.body)
	}
	// Plain HTTP upstream: the payload must be hashed, not UNSIGNED-PAYLOAD.
	if got.contentSha == "UNSIGNED-PAYLOAD" || len(got.contentSha) != 64 {
		t.Errorf("content sha256 = %q", got.contentSha)
	}
	if got := w.Header().Get("x-obs-meta-owner"); got != "alice" {
		t.Errorf("response metadata = %q", got)
	}
	if got := w.Header().Get("x-obs-request-id"); got == "" {
		t.Error("missing x-obs-request-id")
	}
}

// TestProxyHTTPSUpstreamStreams checks that a TLS upstream gets the payload
// streamed through with UNSIGNED-PAYLOAD.
func TestProxyHTTPSUpstreamStreams(t *testing.T) {
	var sha string
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sha = r.Header.Get("x-amz-content-sha256")
		io.Copy(io.Discard, r.Body)
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	g := testGateway(t, upstream.URL, "", "")
	g.client.Transport = upstream.Client().Transport

	r := httptest.NewRequest(http.MethodPut, "http://legacy-bucket.obs.test/big.bin", strings.NewReader("data"))
	r.Host = "legacy-bucket.obs.test"
	w := httptest.NewRecorder()
	g.ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	if sha != "UNSIGNED-PAYLOAD" {
		t.Fatalf("content sha256 = %q, want UNSIGNED-PAYLOAD", sha)
	}
}

func TestSignatureRequired(t *testing.T) {
	g := testGateway(t, "http://127.0.0.1:9000", "AKTEST", "SKTEST")

	r := httptest.NewRequest(http.MethodGet, "http://my-bucket.obs.test/obj.txt", nil)
	r.Host = "my-bucket.obs.test"
	w := httptest.NewRecorder()
	g.ServeHTTP(w, r)

	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "AccessDenied") {
		t.Fatalf("body = %s", w.Body.String())
	}
}

func TestNotImplementedExtension(t *testing.T) {
	g := testGateway(t, "http://127.0.0.1:9000", "", "")

	r := httptest.NewRequest(http.MethodPost, "http://my-bucket.obs.test/obj?append&position=0", nil)
	r.Host = "my-bucket.obs.test"
	w := httptest.NewRecorder()
	g.ServeHTTP(w, r)

	if w.Code != http.StatusNotImplemented {
		t.Fatalf("status = %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "NotImplemented") {
		t.Fatalf("body = %s", w.Body.String())
	}
}
