// Command dump is a protocol capture server: it prints every request it
// receives and answers the way OBS does, so the real SDK can be driven against
// it. Used to produce docs/protocol.md; not part of the adapter.
package main

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strings"
)

func main() {
	addr := "127.0.0.1:18081"
	if len(os.Args) > 1 {
		addr = os.Args[1]
	}
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)

		var b strings.Builder
		fmt.Fprintf(&b, "\n=== %s %s  (host=%s)\n", r.Method, r.URL.RequestURI(), r.Host)
		names := make([]string, 0, len(r.Header))
		for name := range r.Header {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			fmt.Fprintf(&b, "  %s: %s\n", name, strings.Join(r.Header[name], " | "))
		}
		if len(body) > 0 {
			sum := sha256.Sum256(body)
			fmt.Fprintf(&b, "  [body] len=%d sha256=%s\n", len(body), hex.EncodeToString(sum[:]))
		}
		os.Stdout.WriteString(b.String())

		w.Header().Set("x-obs-api", "3.0")
		w.Header().Set("x-obs-request-id", "0001")
		w.Header().Set("x-obs-id-2", "id2")
		switch {
		case r.URL.Query().Has("apiversion"):
			w.WriteHeader(http.StatusOK)
		case r.Method == http.MethodPut:
			w.Header().Set("ETag", `"9b2cf535f27731c974343645a3985328"`)
			w.WriteHeader(http.StatusOK)
		case r.Method == http.MethodGet && r.URL.Path != "/" && !r.URL.Query().Has("prefix"):
			w.Header().Set("Content-Type", "application/octet-stream")
			w.Header().Set("Content-Length", "5")
			w.Header().Set("ETag", `"9b2cf535f27731c974343645a3985328"`)
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("hello"))
		case r.Method == http.MethodHead:
			w.Header().Set("Content-Length", "5")
			w.Header().Set("ETag", `"9b2cf535f27731c974343645a3985328"`)
			w.WriteHeader(http.StatusOK)
		case r.Method == http.MethodDelete:
			w.WriteHeader(http.StatusNoContent)
		default:
			w.Header().Set("Content-Type", "application/xml")
			w.WriteHeader(http.StatusOK)
		}
	})
	fmt.Fprintln(os.Stderr, "dump server listening on", addr)
	if err := http.ListenAndServe(addr, nil); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
