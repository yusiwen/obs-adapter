package gateway

import (
	"net/http"
	"strings"
)

// obsToAmz maps OBS request header names onto their S3 equivalents.
var obsToAmz = map[string]string{
	"x-obs-acl":                                       "x-amz-acl",
	"x-obs-grant-read":                                "x-amz-grant-read",
	"x-obs-grant-write":                               "x-amz-grant-write",
	"x-obs-grant-read-acp":                            "x-amz-grant-read-acp",
	"x-obs-grant-write-acp":                           "x-amz-grant-write-acp",
	"x-obs-grant-full-control":                        "x-amz-grant-full-control",
	"x-obs-copy-source":                               "x-amz-copy-source",
	"x-obs-copy-source-range":                         "x-amz-copy-source-range",
	"x-obs-copy-source-if-match":                      "x-amz-copy-source-if-match",
	"x-obs-copy-source-if-none-match":                 "x-amz-copy-source-if-none-match",
	"x-obs-copy-source-if-modified-since":             "x-amz-copy-source-if-modified-since",
	"x-obs-copy-source-if-unmodified-since":           "x-amz-copy-source-if-unmodified-since",
	"x-obs-metadata-directive":                        "x-amz-metadata-directive",
	"x-obs-storage-class":                             "x-amz-storage-class",
	"x-obs-tagging":                                   "x-amz-tagging",
	"x-obs-website-redirect-location":                 "x-amz-website-redirect-location",
	"x-obs-security-token":                            "x-amz-security-token",
	"x-obs-version-id":                                "x-amz-version-id",
	"x-obs-server-side-encryption":                    "x-amz-server-side-encryption",
	"x-obs-server-side-encryption-kms-key-id":         "x-amz-server-side-encryption-aws-kms-key-id",
	"x-obs-server-side-encryption-customer-algorithm": "x-amz-server-side-encryption-customer-algorithm",
	"x-obs-server-side-encryption-customer-key":       "x-amz-server-side-encryption-customer-key",
	"x-obs-server-side-encryption-customer-key-md5":   "x-amz-server-side-encryption-customer-key-md5",
	"x-obs-expires":                                   "x-amz-expiration",
	"x-obs-object-type":                               "x-amz-object-type",
	"x-obs-request-payer":                             "x-amz-request-payer",
}

// amzToObs maps upstream response header names onto what the OBS SDK reads.
var amzToObs = map[string]string{
	"x-amz-request-id":                                "x-obs-request-id",
	"x-amz-id-2":                                      "x-obs-id-2",
	"x-amz-version-id":                                "x-obs-version-id",
	"x-amz-delete-marker":                             "x-obs-delete-marker",
	"x-amz-storage-class":                             "x-obs-storage-class",
	"x-amz-server-side-encryption":                    "x-obs-server-side-encryption",
	"x-amz-server-side-encryption-aws-kms-key-id":     "x-obs-server-side-encryption-kms-key-id",
	"x-amz-server-side-encryption-customer-algorithm": "x-obs-server-side-encryption-customer-algorithm",
	"x-amz-server-side-encryption-customer-key-md5":   "x-obs-server-side-encryption-customer-key-md5",
	"x-amz-restore":                                   "x-obs-restore",
	"x-amz-expiration":                                "x-obs-expiration",
	"x-amz-website-redirect-location":                 "x-obs-website-redirect-location",
	"x-amz-object-type":                               "x-obs-object-type",
	"x-amz-tagging-count":                             "x-obs-tagging-count",
	"x-amz-replication-status":                        "x-obs-replication-status",
	"x-amz-mp-parts-count":                            "x-obs-mp-parts-count",
	"x-amz-request-charged":                           "x-obs-request-charged",
}

// passThroughResponse keeps only headers that carry the object semantics the
// SDK exposes to the application.
var passThroughResponse = map[string]bool{
	"content-type":        true,
	"content-length":      true,
	"content-range":       true,
	"content-encoding":    true,
	"content-language":    true,
	"content-disposition": true,
	"cache-control":       true,
	"expires":             true,
	"etag":                true,
	"last-modified":       true,
	"accept-ranges":       true,
	"date":                true,
	"location":            true,
	"allow":               true,
	"retry-after":         true,
}

// requestHeader returns the headers to forward upstream: hop-by-hop and
// authentication headers are dropped, x-obs-* names are translated.
func requestHeader(in http.Header) http.Header {
	out := http.Header{}
	for name, values := range in {
		lower := strings.ToLower(name)
		switch {
		case lower == "authorization", lower == "host", lower == "connection",
			lower == "keep-alive", lower == "proxy-authenticate", lower == "proxy-authorization",
			lower == "te", lower == "trailer", lower == "transfer-encoding", lower == "upgrade",
			lower == "expect", lower == "user-agent", lower == "accept-encoding":
			continue
		case strings.HasPrefix(lower, "x-obs-meta-"):
			out[http.CanonicalHeaderKey("x-amz-meta-"+lower[len("x-obs-meta-"):])] = values
		case strings.HasPrefix(lower, "x-obs-"):
			if amz, ok := obsToAmz[lower]; ok {
				out[http.CanonicalHeaderKey(amz)] = values
			}
			// Unknown x-obs-* headers are OBS server extensions; dropping them
			// is safe because the object payload does not depend on them.
		default:
			out[http.CanonicalHeaderKey(name)] = values
		}
	}
	return out
}

// responseHeader converts the upstream headers into the OBS dialect.
func responseHeader(dst http.Header, src http.Header, reqID string) {
	for name, values := range src {
		lower := strings.ToLower(name)
		if obs, ok := amzToObs[lower]; ok {
			dst[http.CanonicalHeaderKey(obs)] = values
			continue
		}
		if strings.HasPrefix(lower, "x-amz-meta-") {
			dst[http.CanonicalHeaderKey("x-obs-meta-"+lower[len("x-amz-meta-"):])] = values
			continue
		}
		if strings.HasPrefix(lower, "x-amz-") {
			// Remaining x-amz-* headers have no OBS counterpart.
			continue
		}
		if !passThroughResponse[lower] {
			continue
		}
		dst[http.CanonicalHeaderKey(name)] = values
	}
	dst.Set("x-obs-request-id", reqID)
	dst.Set("x-obs-id-2", "obs-adapter")
	dst.Set("x-obs-api", "3.0")
}
