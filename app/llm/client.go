// Package llm calls models through the gateway. It knows the gateway only by
// its address, and models only by their logical names.
package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// The logical model names, the only ones the app may use. The gateway's
// config maps each to an upstream and that upstream's own name for it.
const (
	Extract = "extract" // a sentence into a structured record: fast, called often
	Chat    = "chat"    // questions that need reasoning: slow, called rarely
	Embed   = "embed"   // text into vectors
)

// Client calls the gateway's OpenAI-compatible API.
type Client struct {
	baseURL string
	key     string
	http    *http.Client
}

// New returns a client for the gateway at baseURL, which sends key with
// every request.
func New(baseURL, key string) *Client {
	return &Client{
		baseURL: strings.TrimSuffix(baseURL, "/"),
		key:     key,
		// The gateway gives up on an upstream after 120 s, so this only
		// catches a gateway that hangs.
		http: &http.Client{Timeout: 150 * time.Second},
	}
}

// Message is one turn of a chat.
type Message struct {
	Role    string `json:"role"` // "system", "user" or "assistant"
	Content string `json:"content"`
}

// ChatRequest is a non-streaming chat request.
type ChatRequest struct {
	Model    string    `json:"model"`
	Messages []Message `json:"messages"`
}

// Error is an answer other than 200, from the gateway or from the upstream
// behind it. Both use OpenAI's error shape.
type Error struct {
	Status  int
	Type    string
	Message string
}

func (e *Error) Error() string {
	return fmt.Sprintf("gateway answered %d (%s): %s", e.Status, e.Type, e.Message)
}

// Chat sends req and returns the text of the reply. feature names the part
// of the app that is asking; the gateway logs it.
func (c *Client) Chat(ctx context.Context, feature string, req ChatRequest) (string, error) {
	var resp struct {
		Choices []struct {
			Message Message `json:"message"`
		} `json:"choices"`
	}
	if err := c.post(ctx, "/v1/chat/completions", feature, req, &resp); err != nil {
		return "", err
	}
	if len(resp.Choices) == 0 {
		return "", fmt.Errorf("chat: the reply has no choices")
	}
	return resp.Choices[0].Message.Content, nil
}

// Embed returns a vector for each input, in the order of the inputs. There is
// only one embedding model: vectors from different models cannot be compared.
func (c *Client) Embed(ctx context.Context, feature string, inputs []string) ([][]float32, error) {
	req := struct {
		Model string   `json:"model"`
		Input []string `json:"input"`
	}{Embed, inputs}
	var resp struct {
		Data []struct {
			Index     int       `json:"index"`
			Embedding []float32 `json:"embedding"`
		} `json:"data"`
	}
	if err := c.post(ctx, "/v1/embeddings", feature, req, &resp); err != nil {
		return nil, err
	}
	if len(resp.Data) != len(inputs) {
		return nil, fmt.Errorf("embed: %d vectors for %d inputs", len(resp.Data), len(inputs))
	}
	// Each vector says which input it is for; the order of the list is not
	// promised.
	out := make([][]float32, len(inputs))
	for _, d := range resp.Data {
		if d.Index < 0 || d.Index >= len(out) || out[d.Index] != nil {
			return nil, fmt.Errorf("embed: bad or repeated index %d", d.Index)
		}
		out[d.Index] = d.Embedding
	}
	return out, nil
}

// Ready reports whether the gateway answers its readiness probe.
func (c *Client) Ready(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, "GET", c.baseURL+"/readyz", nil)
	if err != nil {
		return err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("readyz: %s", resp.Status)
	}
	return nil
}

func (c *Client) post(ctx context.Context, path, feature string, body, out any) error {
	b, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, "POST", c.baseURL+path, bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.key)
	req.Header.Set("X-Feature", feature)
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return readError(resp)
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("%s: decoding the reply: %w", path, err)
	}
	return nil
}

// readError turns an answer other than 200 into an *Error.
func readError(resp *http.Response) error {
	e := &Error{Status: resp.StatusCode, Type: "unknown", Message: http.StatusText(resp.StatusCode)}
	var body struct {
		Error struct {
			Message string `json:"message"`
			Type    string `json:"type"`
		} `json:"error"`
	}
	// An error body is short; the limit only guards against something that
	// is not the gateway.
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if json.Unmarshal(b, &body) == nil && body.Error.Message != "" {
		e.Message = body.Error.Message
		if body.Error.Type != "" {
			e.Type = body.Error.Type
		}
	}
	return e
}
