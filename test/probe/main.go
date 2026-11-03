// Command probe checks that the configured MinIO endpoint and credentials work
// by signing a few SigV4 requests with the adapter's own signer. It never
// prints the credentials.
package main

import (
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"time"

	"github.com/yusiwen/obs-adapter/internal/s3sig"
)

func creds() s3sig.Credentials {
	return s3sig.Credentials{
		AccessKey: os.Getenv("OBS_ADAPTER_MINIO_AK"),
		SecretKey: os.Getenv("OBS_ADAPTER_MINIO_SK"),
		Region:    orDefault(os.Getenv("OBS_ADAPTER_MINIO_REGION"), "us-east-1"),
	}
}

func orDefault(v, def string) string {
	if v == "" {
		return def
	}
	return v
}

func main() {
	ep := os.Getenv("OBS_ADAPTER_MINIO_ENDPOINT")
	if ep == "" {
		fmt.Println("OBS_ADAPTER_MINIO_ENDPOINT is not set")
		os.Exit(2)
	}
	bucket := "test"
	if len(os.Args) > 1 {
		bucket = os.Args[1]
	}

	base, err := url.Parse(ep)
	if err != nil {
		fmt.Println("bad endpoint:", err)
		os.Exit(2)
	}
	fmt.Printf("endpoint=%s bucket=%s region=%s\n", ep, bucket, creds().Region)

	cases := []struct {
		name   string
		method string
		path   string
		query  url.Values
	}{
		{"bucket location", http.MethodGet, "/" + bucket, url.Values{"location": {""}}},
		{"head bucket", http.MethodHead, "/" + bucket, nil},
		{"list objects", http.MethodGet, "/" + bucket, url.Values{"max-keys": {"5"}}},
	}

	client := &http.Client{Timeout: 20 * time.Second}
	for _, c := range cases {
		u := *base
		u.Path = c.path
		u.RawPath = s3sig.EncodePath(c.path)
		u.RawQuery = c.query.Encode()

		req, err := http.NewRequest(c.method, "http://placeholder", nil)
		if err != nil {
			fmt.Println(c.name, "build:", err)
			continue
		}
		req.URL = &u
		req.Host = u.Host
		req.Header = http.Header{}
		s3sig.Sign(req, creds(), s3sig.EmptyPayload, time.Now())

		resp, err := client.Do(req)
		if err != nil {
			fmt.Printf("%-16s ERROR %v\n", c.name, err)
			continue
		}
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 200))
		resp.Body.Close()
		fmt.Printf("%-16s %d %s %s\n", c.name, resp.StatusCode, resp.Header.Get("Content-Type"),
			string(body))
	}
}
