package gateway

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
)

// apiError is OpenAI's error shape, so callers can parse the gateway's own
// errors and an upstream's with the same code.
type apiError struct {
	Message string `json:"message"`
	Type    string `json:"type"`
	Code    string `json:"code"`
}

// writeError sends an error the gateway itself produced. Callers branch on
// the status and code; the type only tells their mistakes from ours.
func writeError(w http.ResponseWriter, status int, code, message string) {
	typ := "invalid_request_error"
	if status >= 500 {
		typ = "server_error"
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]apiError{
		"error": {Message: message, Type: typ, Code: code},
	})
}

// writeStreamError ends a stream that broke off with one last event. OpenAI's
// SDK raises an exception for an event whose data has an "error" key.
func writeStreamError(w io.Writer, code, message string) {
	// Marshal cannot fail on strings.
	data, _ := json.Marshal(map[string]apiError{
		"error": {Message: message, Type: "server_error", Code: code},
	})
	fmt.Fprintf(w, "event: error\ndata: %s\n\n", data)
}
