package server

import (
	"net/http"
	"strings"
	"testing"
)

// TestUIAPIHelpersFallBackToHTTPStatus pins the UI request helpers reading the
// body as text and parsing it defensively. An unconditional r.json() rejects
// with a SyntaxError on ingress HTML 502/503/504 or ServeMux plain-text
// 404/405 before the 'HTTP ' + r.status fallback can run (#586).
func TestUIAPIHelpersFallBackToHTTPStatus(t *testing.T) {
	h := newTestServer(t, "/")
	rec := doJSON(t, h, "GET", "/", "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("UI page: %d", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{
		"function parseResponse(r)",
		"r.text().then(",
		"'HTTP ' + r.status",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("UI page missing %q", want)
		}
	}
	if strings.Contains(body, "return r.json().then(") {
		t.Error("UI helpers must not call r.json() unconditionally (#586)")
	}
}
