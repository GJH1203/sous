package fakeupstream

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

type chatRequest struct {
	Model    string `json:"model"`
	Messages []struct {
		Content json.RawMessage `json:"content"`
	} `json:"messages"`
	Stream        bool `json:"stream"`
	StreamOptions struct {
		IncludeUsage bool `json:"include_usage"`
	} `json:"stream_options"`
}

type chatCompletion struct {
	ID      string   `json:"id"`
	Object  string   `json:"object"`
	Created int64    `json:"created"`
	Model   string   `json:"model"`
	Choices []choice `json:"choices"`
	Usage   *usage   `json:"usage,omitempty"`
}

type choice struct {
	Index        int      `json:"index"`
	Message      *message `json:"message,omitempty"`
	Delta        *message `json:"delta,omitempty"`
	FinishReason *string  `json:"finish_reason"`
}

type message struct {
	Role    string `json:"role,omitempty"`
	Content string `json:"content,omitempty"`
}

type usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens,omitempty"`
	TotalTokens      int `json:"total_tokens"`
}

func (s *Server) chat(w http.ResponseWriter, r *http.Request, c call, body []byte) {
	var req chatRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error(), "invalid_request_error")
		return
	}
	if len(req.Messages) == 0 {
		writeError(w, http.StatusBadRequest, `"messages" must be a non-empty array`, "invalid_request_error")
		return
	}
	prompt := 0
	for _, m := range req.Messages {
		prompt += tokens(m.Content)
	}
	parts := split(c.cfg)
	out := chatCompletion{
		ID:      fmt.Sprintf("chatcmpl-fake-%d", c.seq),
		Created: time.Now().Unix(),
		Model:   req.Model,
	}
	u := &usage{PromptTokens: prompt, CompletionTokens: len(parts), TotalTokens: prompt + len(parts)}

	if req.Stream {
		s.stream(w, r, c.cfg, out, parts, u, req.StreamOptions.IncludeUsage)
		return
	}
	// A real server sends nothing until the whole reply is generated.
	s.pause(r.Context(), c.cfg.PromptDelay+time.Duration(len(parts))*c.cfg.ChunkInterval)
	stop := "stop"
	out.Object = "chat.completion"
	out.Choices = []choice{{Message: &message{Role: "assistant", Content: strings.Join(parts, "")}, FinishReason: &stop}}
	out.Usage = u
	writeReply(w, c.cfg, out)
	s.countTokens(u.PromptTokens, u.CompletionTokens)
}

// stream sends the reply as server-sent events in llama-server's order: a
// role chunk, the content chunks, a chunk with finish_reason, a usage chunk
// if asked for, then [DONE].
func (s *Server) stream(w http.ResponseWriter, r *http.Request, cfg Config, out chatCompletion, parts []string, u *usage, includeUsage bool) {
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	rc := http.NewResponseController(w)
	if cfg.PromptDelay > 0 {
		// The headers go out on their own, before any event.
		rc.Flush()
		s.pause(r.Context(), cfg.PromptDelay)
	}
	out.Object = "chat.completion.chunk"
	send := func(choices []choice, u *usage) {
		out.Choices, out.Usage = choices, u
		b, err := json.Marshal(out)
		if err != nil {
			panic(err)
		}
		if _, err := fmt.Fprintf(w, "data: %s\n\n", b); err != nil {
			abort()
		}
		// Without a flush, chunks sit in the server's buffer and reach the
		// caller in bursts, which is the bug a proxy test is looking for.
		if err := rc.Flush(); err != nil {
			abort()
		}
	}
	breakAt := func(sent int) {
		if cfg.BreakAfter > 0 && sent == cfg.BreakAfter {
			abort()
		}
	}

	send([]choice{{Delta: &message{Role: "assistant"}}}, nil)
	for i, p := range parts {
		breakAt(i)
		s.pause(r.Context(), cfg.ChunkInterval)
		send([]choice{{Delta: &message{Content: p}}}, nil)
	}
	breakAt(len(parts))
	stop := "stop"
	send([]choice{{Delta: &message{}, FinishReason: &stop}}, nil)
	if includeUsage {
		// An empty array, not null: that is what OpenAI and llama-server send.
		send([]choice{}, u)
	}
	fmt.Fprint(w, "data: [DONE]\n\n")
	rc.Flush()
	s.countTokens(u.PromptTokens, u.CompletionTokens)
}

type embeddingRequest struct {
	Model string          `json:"model"`
	Input json.RawMessage `json:"input"`
}

type embeddingList struct {
	Object string      `json:"object"`
	Data   []embedding `json:"data"`
	Model  string      `json:"model"`
	Usage  usage       `json:"usage"`
}

type embedding struct {
	Object    string    `json:"object"`
	Index     int       `json:"index"`
	Embedding []float32 `json:"embedding"`
}

func (s *Server) embeddings(w http.ResponseWriter, r *http.Request, c call, body []byte) {
	var req embeddingRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error(), "invalid_request_error")
		return
	}
	inputs, ok := parseInput(req.Input)
	if !ok {
		writeError(w, http.StatusBadRequest, `"input" must be a string or a non-empty array of strings`, "invalid_request_error")
		return
	}
	out := embeddingList{Object: "list", Model: req.Model, Data: make([]embedding, len(inputs))}
	for i, in := range inputs {
		out.Data[i] = embedding{Object: "embedding", Index: i, Embedding: vector(in, c.cfg.Dims)}
		out.Usage.PromptTokens += utf8.RuneCountInString(in)
	}
	out.Usage.TotalTokens = out.Usage.PromptTokens
	s.pause(r.Context(), c.cfg.PromptDelay)
	writeReply(w, c.cfg, out)
	s.countTokens(out.Usage.PromptTokens, 0)
}

func parseInput(raw json.RawMessage) ([]string, bool) {
	// A missing input and an explicit null both mean "no input".
	if len(raw) == 0 || string(raw) == "null" {
		return nil, false
	}
	var one string
	if json.Unmarshal(raw, &one) == nil {
		return []string{one}, true
	}
	var many []string
	if json.Unmarshal(raw, &many) == nil && len(many) > 0 {
		return many, true
	}
	return nil, false
}

// vector derives a unit-length vector from the text alone, so equal texts get
// equal vectors and different texts almost surely do not. Unlike a real
// model's, nearby meanings do not give nearby vectors.
func vector(text string, dims int) []float32 {
	v := make([]float64, dims)
	var block [sha256.Size]byte
	norm := 0.0
	for i := range v {
		if i%4 == 0 {
			block = sha256.Sum256(fmt.Appendf(nil, "%d:%s", i/4, text))
		}
		u := binary.BigEndian.Uint64(block[(i%4)*8:])
		v[i] = float64(u>>11)/(1<<53)*2 - 1
		norm += v[i] * v[i]
	}
	norm = math.Sqrt(norm)
	out := make([]float32, dims)
	for i, x := range v {
		out[i] = float32(x / norm)
	}
	return out
}

// split returns the content of each chunk.
func split(cfg Config) []string {
	if cfg.Reply == "" {
		parts := make([]string, cfg.Chunks)
		for i := range parts {
			parts[i] = fmt.Sprintf(" w%d", i)
		}
		parts[0] = "w0"
		return parts
	}
	// Split by characters, not bytes, so no chunk holds half of a 番.
	runes := []rune(cfg.Reply)
	n := min(cfg.Chunks, len(runes))
	parts := make([]string, n)
	for i := range parts {
		parts[i] = string(runes[i*len(runes)/n : (i+1)*len(runes)/n])
	}
	return parts
}

// tokens counts one token per character of a message's content: a string's
// characters, or the raw JSON of anything else.
func tokens(content json.RawMessage) int {
	var s string
	if json.Unmarshal(content, &s) == nil {
		return utf8.RuneCountInString(s)
	}
	return utf8.RuneCount(content)
}

// writeReply writes a complete, non-streamed reply, or with BreakAfter set,
// half of one followed by a cut connection.
func writeReply(w http.ResponseWriter, cfg Config, v any) {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Content-Length", strconv.Itoa(len(b)))
	w.WriteHeader(http.StatusOK)
	if cfg.BreakAfter > 0 {
		w.Write(b[:len(b)/2])
		http.NewResponseController(w).Flush()
		abort()
	}
	w.Write(b)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	// An error here means the caller has gone; there is no one to tell.
	json.NewEncoder(w).Encode(v)
}

// writeError writes llama-server's error shape. Note that code is a number,
// where OpenAI's is a string.
func writeError(w http.ResponseWriter, status int, msg, typ string) {
	type detail struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
		Type    string `json:"type"`
	}
	writeJSON(w, status, struct {
		Error detail `json:"error"`
	}{detail{status, msg, typ}})
}

func errorType(status int) string {
	switch {
	case status == 401:
		return "authentication_error"
	case status == 403:
		return "permission_error"
	case status == 404:
		return "not_found_error"
	case status == 429:
		return "rate_limit_error"
	case status == 503:
		return "unavailable_error"
	case status < 500:
		return "invalid_request_error"
	default:
		return "server_error"
	}
}
