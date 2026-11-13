package web

import (
	"context"
	"io"
	"net/http"
)

// Fetch GETs url and returns the status code and body. It waits as long as
// client.Timeout and ctx allow, and the zero http.Client has no timeout: if
// the server never answers and ctx has no deadline, Fetch never returns.
func Fetch(ctx context.Context, client *http.Client, url string) (int, string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return 0, "", err
	}
	resp, err := client.Do(req)
	if err != nil {
		return 0, "", err
	}
	// Always close the body. One left open and unread ties up its
	// connection, and the goroutines serving it, for good.
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return 0, "", err
	}
	return resp.StatusCode, string(body), nil
}
