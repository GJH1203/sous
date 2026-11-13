package web

import (
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// httptest.NewRecorder stands in for the network: the handler is called as
// a plain method and writes into memory. No port, no client, no goroutines.
func TestRoutes(t *testing.T) {
	h := Routes(time.Hour) // /slow is not called here
	huge := `"` + strings.Repeat("x", maxEchoBytes) + `"`

	tests := []struct {
		name         string
		method, path string
		body         string
		wantStatus   int
		wantBody     string // checked only if not empty
		wantType     string // checked only if not empty
	}{
		{"health", "GET", "/healthz", "", 200, "ok\n", ""},
		{"echo keeps the exact bytes", "POST", "/echo", `{"b": 1, "a": [1.50]}`, 200, `{"b": 1, "a": [1.50]}`, "application/json"},
		{"echo rejects broken JSON", "POST", "/echo", `{"a":`, 400, "", ""},
		{"echo rejects an empty body", "POST", "/echo", "", 400, "", ""},
		{"echo rejects a huge body", "POST", "/echo", huge, 413, "", ""},
		{"wrong method", "GET", "/echo", "", 405, "", ""},
		{"unknown path", "GET", "/nope", "", 404, "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(tt.method, tt.path, strings.NewReader(tt.body))
			rec := httptest.NewRecorder()

			h.ServeHTTP(rec, req)

			if rec.Code != tt.wantStatus {
				t.Errorf("status = %d, want %d", rec.Code, tt.wantStatus)
			}
			if got := rec.Body.String(); tt.wantBody != "" && got != tt.wantBody {
				t.Errorf("body = %q, want %q", got, tt.wantBody)
			}
			// Result() has the headers as they were sent. rec.Header() is the
			// live map the handler writes into, and would also show changes
			// made after the first Write, too late to reach a real client.
			if got := rec.Result().Header.Get("Content-Type"); tt.wantType != "" && got != tt.wantType {
				t.Errorf("Content-Type = %q, want %q", got, tt.wantType)
			}
		})
	}
}
