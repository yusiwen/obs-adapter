// Package config holds the runtime configuration of the OBS adapter.
//
// Every field can be provided either as a command line flag or through an
// environment variable (OBS_ADAPTER_*). Flags win over environment variables.
package config

import (
	"flag"
	"fmt"
	"os"
	"strings"
	"time"
)

// Config is the complete adapter configuration.
type Config struct {
	// Listen is the address the OBS API is served on, e.g. ":8080" or "0.0.0.0:443".
	Listen string
	// TLSCert / TLSKey enable HTTPS when both are set.
	TLSCert string
	TLSKey  string

	// BaseDomain enables virtual hosted style addressing: a request for
	// "my-bucket.obs.example.com" is routed to bucket "my-bucket".
	//
	// The OBS Java SDK (new ObsClient(ak, sk, endpoint)) always uses virtual
	// hosted style, so in production this must be set to the endpoint the
	// Java application is configured with.
	BaseDomain string

	// MinioEndpoint is the upstream S3 endpoint, e.g. http://127.0.0.1:9000.
	MinioEndpoint  string
	MinioAccessKey string
	MinioSecretKey string
	MinioRegion    string
	// MinioPathPrefix lets the adapter live behind a gateway that adds a path prefix.
	MinioPathPrefix string

	// ObsAccessKey / ObsSecretKey are the credentials clients authenticate
	// with. When ObsAccessKey is empty, request signatures are not verified
	// (development only).
	ObsAccessKey string
	ObsSecretKey string
	// MaxSkew is the tolerated difference between the request Date header and
	// the adapter clock. Zero disables the check.
	MaxSkew time.Duration

	// AutoCreateBucket creates the upstream bucket on first write.
	AutoCreateBucket bool
	// BucketAliases maps an OBS bucket name to a different MinIO bucket name.
	BucketAliases map[string]string

	// LogLevel is debug, info, warn or error.
	LogLevel string
}

// Load parses flags and environment variables and validates the result.
func Load(args []string) (*Config, error) {
	c := &Config{}
	aliases := &stringList{}

	fs := flag.NewFlagSet("obs-adapter", flag.ContinueOnError)
	fs.StringVar(&c.Listen, "listen", env("OBS_ADAPTER_LISTEN", ":8080"), "address to serve the OBS API on")
	fs.StringVar(&c.TLSCert, "tls-cert", env("OBS_ADAPTER_TLS_CERT", ""), "TLS certificate file (enables HTTPS)")
	fs.StringVar(&c.TLSKey, "tls-key", env("OBS_ADAPTER_TLS_KEY", ""), "TLS private key file")
	fs.StringVar(&c.BaseDomain, "domain", env("OBS_ADAPTER_DOMAIN", ""), "base domain for virtual hosted style bucket addressing")
	fs.StringVar(&c.MinioEndpoint, "minio", env("OBS_ADAPTER_MINIO_ENDPOINT", "http://127.0.0.1:9000"), "upstream MinIO S3 endpoint")
	fs.StringVar(&c.MinioAccessKey, "minio-ak", env("OBS_ADAPTER_MINIO_AK", ""), "MinIO access key")
	fs.StringVar(&c.MinioSecretKey, "minio-sk", env("OBS_ADAPTER_MINIO_SK", ""), "MinIO secret key")
	fs.StringVar(&c.MinioRegion, "minio-region", env("OBS_ADAPTER_MINIO_REGION", "us-east-1"), "MinIO region used for SigV4")
	fs.StringVar(&c.MinioPathPrefix, "minio-path-prefix", env("OBS_ADAPTER_MINIO_PATH_PREFIX", ""), "path prefix prepended to upstream requests")
	fs.StringVar(&c.ObsAccessKey, "ak", env("OBS_ADAPTER_AK", ""), "access key clients must sign with (empty disables verification)")
	fs.StringVar(&c.ObsSecretKey, "sk", env("OBS_ADAPTER_SK", ""), "secret key clients must sign with")
	fs.DurationVar(&c.MaxSkew, "max-skew", durationEnv("OBS_ADAPTER_MAX_SKEW", 15*time.Minute), "tolerated clock skew for the Date header (0 disables)")
	fs.BoolVar(&c.AutoCreateBucket, "auto-create-bucket", boolEnv("OBS_ADAPTER_AUTO_CREATE_BUCKET", true), "create upstream buckets on first write")
	fs.Var(aliases, "bucket-alias", "bucket name mapping obs=minio (repeatable)")
	fs.StringVar(&c.LogLevel, "log-level", env("OBS_ADAPTER_LOG_LEVEL", "info"), "debug, info, warn or error")
	if err := fs.Parse(args); err != nil {
		return nil, err
	}

	if c.MinioAccessKey == "" || c.MinioSecretKey == "" {
		return nil, fmt.Errorf("minio credentials are required (-minio-ak / -minio-sk)")
	}
	if (c.TLSCert == "") != (c.TLSKey == "") {
		return nil, fmt.Errorf("-tls-cert and -tls-key must be given together")
	}
	if c.ObsAccessKey != "" && c.ObsSecretKey == "" {
		return nil, fmt.Errorf("-sk is required when -ak is set")
	}

	c.BucketAliases = map[string]string{}
	for _, pair := range aliases.values {
		from, to, ok := strings.Cut(pair, "=")
		if !ok || from == "" || to == "" {
			return nil, fmt.Errorf("invalid -bucket-alias %q, want obs=minio", pair)
		}
		c.BucketAliases[from] = to
	}
	c.BaseDomain = strings.Trim(strings.ToLower(c.BaseDomain), ".")
	c.MinioPathPrefix = strings.Trim(c.MinioPathPrefix, "/")

	return c, nil
}

// UpstreamBucket maps an OBS bucket name to the MinIO bucket that stores it.
func (c *Config) UpstreamBucket(name string) string {
	if to, ok := c.BucketAliases[name]; ok {
		return to
	}
	return name
}

type stringList struct{ values []string }

func (s *stringList) String() string { return strings.Join(s.values, ",") }

func (s *stringList) Set(v string) error {
	s.values = append(s.values, v)
	return nil
}

func env(key, def string) string {
	if v, ok := os.LookupEnv(key); ok {
		return v
	}
	return def
}

func boolEnv(key string, def bool) bool {
	v, ok := os.LookupEnv(key)
	if !ok {
		return def
	}
	return v == "1" || strings.EqualFold(v, "true") || strings.EqualFold(v, "yes")
}

func durationEnv(key string, def time.Duration) time.Duration {
	v, ok := os.LookupEnv(key)
	if !ok {
		return def
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return def
	}
	return d
}
