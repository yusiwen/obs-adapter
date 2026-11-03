// Package gateway implements the OBS service API on top of MinIO.
//
// Requests arrive signed with an OBS signature (see package obsauth). The
// adapter re-signs them with AWS Signature V4 and forwards them to the MinIO
// S3 endpoint, translating headers and answering the OBS-only endpoints
// (api version negotiation, location, storage info, ...) itself.
package gateway

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/yusiwen/obs-adapter/internal/config"
	"github.com/yusiwen/obs-adapter/internal/obsauth"
	"github.com/yusiwen/obs-adapter/internal/s3sig"
)

// Version is reported by /__health and the Server header. It is a variable so
// release builds can inject it with -ldflags "-X ...gateway.Version=...".
var Version = "0.1.0"

const obsNamespace = "http://obs.myhuaweicloud.com/doc/2015-06-30/"

// Gateway serves the OBS API.
type Gateway struct {
	cfg      *config.Config
	upstream *url.URL
	client   *http.Client
	creds    s3sig.Credentials
	log      *slog.Logger
	ensured  sync.Map
}

// New builds a Gateway. The upstream connection pool is sized for a
// servlet-style workload (one Java client, many keep-alive connections).
func New(cfg *config.Config, log *slog.Logger) (*Gateway, error) {
	u, err := url.Parse(cfg.MinioEndpoint)
	if err != nil {
		return nil, fmt.Errorf("parse minio endpoint: %w", err)
	}
	if u.Scheme == "" || u.Host == "" {
		return nil, fmt.Errorf("minio endpoint %q must include a scheme and host", cfg.MinioEndpoint)
	}
	u.Path = ""
	u.RawQuery = ""

	transport := &http.Transport{
		DialContext: (&net.Dialer{
			Timeout:   10 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		MaxIdleConns:          256,
		MaxIdleConnsPerHost:   128,
		IdleConnTimeout:       90 * time.Second,
		ExpectContinueTimeout: time.Second,
		ResponseHeaderTimeout: 60 * time.Second,
		ForceAttemptHTTP2:     false,
		// Object payloads must not be transparently gzip decoded: the ETag and
		// Content-Length the client sees have to describe the stored bytes.
		DisableCompression: true,
	}

	return &Gateway{
		cfg:      cfg,
		upstream: u,
		log:      log,
		client:   &http.Client{Transport: transport},
		creds: s3sig.Credentials{
			AccessKey: cfg.MinioAccessKey,
			SecretKey: cfg.MinioSecretKey,
			Region:    cfg.MinioRegion,
		},
	}, nil
}

// route describes where a request points.
type route struct {
	bucket string
	key    string
	// vhost is true when the bucket name came from the Host header, i.e. the
	// client uses virtual hosted style and signed "/<bucket><path>".
	vhost bool
}

func (g *Gateway) route(r *http.Request) route {
	host := strings.ToLower(r.Host)
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	path := r.URL.Path
	if path == "" {
		path = "/"
	}
	if g.cfg.BaseDomain != "" && host != g.cfg.BaseDomain && strings.HasSuffix(host, "."+g.cfg.BaseDomain) {
		return route{
			bucket: strings.TrimSuffix(host, "."+g.cfg.BaseDomain),
			key:    strings.TrimPrefix(path, "/"),
			vhost:  true,
		}
	}
	trimmed := strings.TrimPrefix(path, "/")
	if trimmed == "" {
		return route{}
	}
	bucket, key, _ := strings.Cut(trimmed, "/")
	return route{bucket: bucket, key: key}
}

func (g *Gateway) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	reqID := newRequestID()
	start := time.Now()
	rt := g.route(r)

	if !rt.vhost && (r.URL.Path == "/__health" || r.URL.Path == "/__health/") {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"status":"ok","version":%q,"upstream":%q}`+"\n", Version, g.cfg.MinioEndpoint)
		return
	}

	if g.handleExtension(w, r, rt, reqID) {
		return
	}

	if _, err := obsauth.Verify(r, rt.bucket, g.cfg.ObsAccessKey, g.cfg.ObsSecretKey, g.cfg.MaxSkew, time.Now()); err != nil {
		if e, ok := obsauth.IsError(err); ok {
			g.throw(w, r, reqID, e.Code, e.Message, e.Status)
			return
		}
		code := "AccessDenied"
		if errors.Is(err, obsauth.ErrAnonymous) {
			code = "AccessDenied"
			err = errors.New("anonymous access is not allowed on this endpoint")
		}
		g.throw(w, r, reqID, code, err.Error(), http.StatusForbidden)
		return
	}

	g.proxy(w, r, rt, reqID, start)
}

// proxy forwards the request to MinIO and translates the response.
func (g *Gateway) proxy(w http.ResponseWriter, r *http.Request, rt route, reqID string, start time.Time) {
	// Provision the bucket up front for writes: retrying after a NoSuchBucket
	// answer is not possible because the request body has been streamed to the
	// backend by then and cannot be replayed.
	if g.cfg.AutoCreateBucket && rt.bucket != "" && isWrite(r.Method) {
		if err := g.ensureBucket(r.Context(), rt.bucket); err != nil {
			g.log.Warn("bucket auto creation failed", "request_id", reqID, "bucket", rt.bucket, "error", err)
		}
	}

	resp, err := g.roundTrip(r, rt)
	if err != nil {
		g.log.Error("upstream request failed", "request_id", reqID, "method", r.Method,
			"bucket", rt.bucket, "key", rt.key, "error", err)
		g.throw(w, r, reqID, "InternalError", "the storage backend is unreachable: "+err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	responseHeader(w.Header(), resp.Header, reqID)
	w.WriteHeader(resp.StatusCode)

	written, copyErr := io.Copy(w, resp.Body)
	level := slog.LevelDebug
	if resp.StatusCode >= 400 || copyErr != nil {
		level = slog.LevelWarn
	}
	g.log.Log(r.Context(), level, "request",
		"request_id", reqID, "method", r.Method, "bucket", rt.bucket, "key", rt.key,
		"status", resp.StatusCode, "bytes", written,
		"duration_ms", time.Since(start).Milliseconds(), "error", copyErr)
}

// roundTrip builds and signs the upstream request.
func (g *Gateway) roundTrip(r *http.Request, rt route) (*http.Response, error) {
	target, err := g.upstreamURL(rt)
	if err != nil {
		return nil, err
	}

	var body io.Reader
	payloadHash := s3sig.EmptyPayload
	contentLength := r.ContentLength
	cleanup := func() {}

	switch {
	case r.Body != nil && r.ContentLength != 0 && g.upstream.Scheme == "https":
		// The payload is streamed straight through; MinIO accepts
		// UNSIGNED-PAYLOAD for signed requests over TLS.
		body = r.Body
		payloadHash = s3sig.UnsignedPayload

	case r.Body != nil && r.ContentLength != 0:
		// Over plain HTTP a real SHA-256 is required, so the body has to be
		// spooled to a temporary file to be hashed and then replayed.
		spool, err := spoolBody(r.Body)
		if err != nil {
			return nil, err
		}
		body = spool.file
		payloadHash = spool.sha256
		contentLength = spool.size
		cleanup = spool.close

	case r.Body != nil:
		body = http.NoBody
	}
	defer cleanup()

	upreq, err := http.NewRequestWithContext(r.Context(), r.Method, "http://placeholder", body)
	if err != nil {
		return nil, err
	}
	upreq.URL = target
	upreq.Host = target.Host
	target.RawQuery = r.URL.RawQuery
	upreq.Header = requestHeader(r.Header)
	upreq.ContentLength = contentLength
	if upreq.Header.Get("Content-Type") == "" && contentLength > 0 {
		// The OBS SDK always sends it, but be explicit when a client does not.
		upreq.Header.Set("Content-Type", "application/octet-stream")
	}

	s3sig.Sign(upreq, g.creds, payloadHash, time.Now())
	return g.client.Do(upreq)
}

// spool is a request body captured on disk together with its digest.
type spool struct {
	file   *os.File
	size   int64
	sha256 string
}

func spoolBody(src io.Reader) (*spool, error) {
	f, err := os.CreateTemp("", "obs-adapter-body-*")
	if err != nil {
		return nil, fmt.Errorf("spool request body: %w", err)
	}
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(f, h), src)
	if err != nil {
		f.Close()
		os.Remove(f.Name())
		return nil, fmt.Errorf("spool request body: %w", err)
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		f.Close()
		os.Remove(f.Name())
		return nil, err
	}
	return &spool{file: f, size: n, sha256: hex.EncodeToString(h.Sum(nil))}, nil
}

func (s *spool) close() {
	name := s.file.Name()
	s.file.Close()
	os.Remove(name)
}

// upstreamURL maps an OBS bucket/key onto the MinIO path-style URL.
func (g *Gateway) upstreamURL(rt route) (*url.URL, error) {
	u := *g.upstream
	path := "/"
	if rt.bucket != "" {
		path = "/" + g.cfg.UpstreamBucket(rt.bucket)
		if rt.key != "" {
			path += "/" + rt.key
		}
	}
	if g.cfg.MinioPathPrefix != "" {
		path = "/" + g.cfg.MinioPathPrefix + path
	}
	u.Path = path
	u.RawPath = s3sig.EncodePath(path)
	return &u, nil
}

// ensureBucket creates the upstream bucket once per process.
func (g *Gateway) ensureBucket(ctx context.Context, bucket string) error {
	if _, ok := g.ensured.Load(bucket); ok {
		return nil
	}
	u, err := g.upstreamURL(route{bucket: bucket})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, "http://placeholder", nil)
	if err != nil {
		return err
	}
	req.URL = u
	req.Host = u.Host
	req.Header = http.Header{}
	s3sig.Sign(req, g.creds, s3sig.EmptyPayload, time.Now())

	resp, err := g.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<10))
	switch resp.StatusCode {
	case http.StatusOK, http.StatusConflict:
		g.ensured.Store(bucket, struct{}{})
		return nil
	default:
		return fmt.Errorf("create bucket %s: %s: %s", bucket, resp.Status, strings.TrimSpace(string(body)))
	}
}

// handleExtension answers the OBS specific endpoints that have no S3
// counterpart. It returns true when the request was handled.
func (g *Gateway) handleExtension(w http.ResponseWriter, r *http.Request, rt route, reqID string) bool {
	q := r.URL.Query()

	// Authorisation type negotiation: the Java SDK probes HEAD /?apiversion and
	// switches to the OBS signature only when x-obs-api >= 3.0 is returned.
	if _, ok := q["apiversion"]; ok && (r.Method == http.MethodHead || r.Method == http.MethodGet) {
		w.Header().Set("x-obs-api", "3.0")
		w.Header().Set("x-obs-request-id", reqID)
		w.Header().Set("x-obs-id-2", "obs-adapter")
		w.WriteHeader(http.StatusOK)
		return true
	}

	writeXML := func(v any) {
		payload, err := xml.Marshal(v)
		if err != nil {
			g.throw(w, r, reqID, "InternalError", err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/xml")
		w.Header().Set("x-obs-request-id", reqID)
		w.Header().Set("x-obs-id-2", "obs-adapter")
		w.Header().Set("x-obs-api", "3.0")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(xml.Header))
		_, _ = w.Write(payload)
	}

	if _, ok := q["location"]; ok && r.Method == http.MethodGet {
		writeXML(&locationConstraint{Xmlns: obsNamespace, Value: g.cfg.MinioRegion})
		return true
	}
	if _, ok := q["storageinfo"]; ok {
		writeXML(&storageInfo{Xmlns: obsNamespace})
		return true
	}
	if _, ok := q["quota"]; ok && r.Method == http.MethodGet {
		writeXML(&bucketQuota{Xmlns: obsNamespace})
		return true
	}
	if _, ok := q["storageclass"]; ok && r.Method == http.MethodGet {
		writeXML(&storageClass{Xmlns: obsNamespace, Value: "STANDARD"})
		return true
	}

	for _, feature := range []string{
		"storagepolicy", "az-redundancy", "fileinterface", "append", "rename",
		"truncate", "modify", "directcoldaccess", "mirrorrefresh",
		"mirrorbacktosource", "obsalias", "replication_progress", "inventory",
	} {
		if _, ok := q[feature]; ok {
			g.notImplemented(w, r, reqID, feature)
			return true
		}
	}
	return false
}

func isWrite(method string) bool {
	return method == http.MethodPut || method == http.MethodPost
}

func newRequestID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b[:])
}

type locationConstraint struct {
	XMLName xml.Name `xml:"LocationConstraint"`
	Xmlns   string   `xml:"xmlns,attr"`
	Value   string   `xml:",chardata"`
}

type storageInfo struct {
	XMLName      xml.Name `xml:"StorageInfo"`
	Xmlns        string   `xml:"xmlns,attr"`
	Size         int64    `xml:"Size"`
	ObjectNumber int64    `xml:"ObjectNumber"`
}

type bucketQuota struct {
	XMLName xml.Name `xml:"Quota"`
	Xmlns   string   `xml:"xmlns,attr"`
	Quota   int64    `xml:"Quota"`
}

type storageClass struct {
	XMLName xml.Name `xml:"StorageClass"`
	Xmlns   string   `xml:"xmlns,attr"`
	Value   string   `xml:",chardata"`
}
