package gateway

import (
	"encoding/xml"
	"fmt"
	"net/http"
)

// errorBody is the OBS/S3 error document the SDKs parse.
type errorBody struct {
	XMLName   xml.Name `xml:"Error"`
	Code      string   `xml:"Code"`
	Message   string   `xml:"Message"`
	Resource  string   `xml:"Resource,omitempty"`
	RequestID string   `xml:"RequestId"`
	HostID    string   `xml:"HostId"`
}

// throw writes an OBS style error response.
func (g *Gateway) throw(w http.ResponseWriter, r *http.Request, reqID, code, message string, status int) {
	h := w.Header()
	h.Set("x-obs-request-id", reqID)
	h.Set("x-obs-id-2", "obs-adapter")
	if r.Method == http.MethodHead {
		w.WriteHeader(status)
		return
	}
	body := errorBody{
		Code:      code,
		Message:   message,
		Resource:  r.URL.Path,
		RequestID: reqID,
		HostID:    "obs-adapter",
	}
	payload, err := xml.Marshal(body)
	if err != nil {
		http.Error(w, code, status)
		return
	}
	h.Set("Content-Type", "application/xml")
	h.Set("Content-Length", fmt.Sprint(len(payload)+len(xml.Header)))
	w.WriteHeader(status)
	_, _ = w.Write([]byte(xml.Header))
	_, _ = w.Write(payload)
	g.log.Debug("request rejected", "request_id", reqID, "code", code, "status", status)
}

// notImplemented answers an OBS-only feature that MinIO cannot provide.
func (g *Gateway) notImplemented(w http.ResponseWriter, r *http.Request, reqID, feature string) {
	g.throw(w, r, reqID, "NotImplemented",
		"The "+feature+" operation of the OBS API is not supported by this endpoint; it is backed by MinIO.",
		http.StatusNotImplemented)
}
