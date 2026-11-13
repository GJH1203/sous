package web

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestLogging(t *testing.T) {
	tests := []struct {
		name         string
		method, path string
		body         string
		wantStatus   int
	}{
		{"200 without WriteHeader", "GET", "/healthz", "", 200},
		{"400 from the handler", "POST", "/echo", "not json", 400},
		{"405 from the mux", "DELETE", "/healthz", "", 405},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var logs bytes.Buffer
			logger := slog.New(slog.NewJSONHandler(&logs, nil))
			h := Logging(logger, Routes(time.Hour))
			rec := httptest.NewRecorder()

			h.ServeHTTP(rec, httptest.NewRequest(tt.method, tt.path, strings.NewReader(tt.body)))

			// The middleware only watches; the client must get the same status.
			if rec.Code != tt.wantStatus {
				t.Errorf("response status = %d, want %d", rec.Code, tt.wantStatus)
			}
			var line struct {
				Method   string         `json:"method"`
				Path     string         `json:"path"`
				Status   int            `json:"status"`
				Duration *time.Duration `json:"duration"` // nil if missing
			}
			// Fails on two lines as well as on none: one request, one line.
			if err := json.Unmarshal(logs.Bytes(), &line); err != nil {
				t.Fatalf("log output %q is not one JSON line: %v", logs.String(), err)
			}
			if line.Method != tt.method || line.Path != tt.path || line.Status != tt.wantStatus {
				t.Errorf("logged %s %s %d, want %s %s %d",
					line.Method, line.Path, line.Status, tt.method, tt.path, tt.wantStatus)
			}
			if line.Duration == nil {
				t.Errorf("log line %q has no duration", logs.String())
			}
		})
	}
}
