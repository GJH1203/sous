package fakeupstream_test

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/GJH1203/sous/internal/fakeupstream"
)

// start runs a fake with no pause between chunks unless edit sets one.
func start(t *testing.T, edit func(*fakeupstream.Config)) (*fakeupstream.Server, string) {
	t.Helper()
	cfg := fakeupstream.DefaultConfig()
	cfg.ChunkInterval = 0
	if edit != nil {
		edit(&cfg)
	}
	fake, err := fakeupstream.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(fake)
	t.Cleanup(func() {
		fake.Close()
		ts.Close()
	})
	return fake, ts.URL
}

func do(t *testing.T, ctx context.Context, method, url, body string) (*http.Response, error) {
	t.Helper()
	req, err := http.NewRequestWithContext(ctx, method, url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	return http.DefaultClient.Do(req)
}

// call sends a request and returns the status, headers and whole body.
func call(t *testing.T, method, url, body string) (int, http.Header, []byte) {
	t.Helper()
	resp, err := do(t, context.Background(), method, url, body)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, resp.Header, b
}

func decode[T any](t *testing.T, b []byte) T {
	t.Helper()
	var v T
	if err := json.Unmarshal(b, &v); err != nil {
		t.Fatalf("decode %s: %v", b, err)
	}
	return v
}

// waitFor polls cond, because a handler finishes a moment after its caller
// has given up.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

const chatBody = `{"model":"m1","messages":[{"role":"user","content":"你好 world"}]}`

type completion struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	Model   string `json:"model"`
	Choices []struct {
		Message      struct{ Role, Content string } `json:"message"`
		Delta        struct{ Role, Content string } `json:"delta"`
		FinishReason *string                        `json:"finish_reason"`
	} `json:"choices"`
	Usage *struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
		TotalTokens      int `json:"total_tokens"`
	} `json:"usage"`
}

type errorBody struct {
	Error struct {
		// An int, as llama-server sends it: a quoted "400" fails to decode.
		Code    int    `json:"code"`
		Message string `json:"message"`
		Type    string `json:"type"`
	} `json:"error"`
}

func TestChat(t *testing.T) {
	fake, url := start(t, func(c *fakeupstream.Config) { c.Name = "A" })
	status, header, body := call(t, "POST", url+"/v1/chat/completions", chatBody)
	if status != 200 {
		t.Fatalf("status %d: %s", status, body)
	}
	if got := header.Get("X-Fake-Upstream"); got != "A" {
		t.Errorf("X-Fake-Upstream = %q, want A", got)
	}
	got := decode[completion](t, body)
	if got.Object != "chat.completion" || got.Model != "m1" {
		t.Errorf("object, model = %q, %q", got.Object, got.Model)
	}
	if len(got.Choices) != 1 {
		t.Fatalf("got %d choices", len(got.Choices))
	}
	ch := got.Choices[0]
	if ch.Message.Role != "assistant" || ch.Message.Content != "w0 w1 w2 w3 w4" {
		t.Errorf("message = %+v", ch.Message)
	}
	if ch.FinishReason == nil || *ch.FinishReason != "stop" {
		t.Errorf("finish_reason = %v", ch.FinishReason)
	}
	// "你好 world" is 8 characters; the default reply is 5 chunks.
	if u := got.Usage; u == nil || u.PromptTokens != 8 || u.CompletionTokens != 5 || u.TotalTokens != 13 {
		t.Errorf("usage = %+v", got.Usage)
	}
	if s := fake.Stats(); s.Requests != 1 || s.PromptTokens != 8 || s.CompletionTokens != 5 || s.Concurrent != 0 {
		t.Errorf("stats = %+v", s)
	}
}

// readStream reads server-sent events until [DONE] or an error.
func readStream(body io.Reader) (events []string, err error) {
	sc := bufio.NewScanner(body)
	for sc.Scan() {
		data, ok := strings.CutPrefix(sc.Text(), "data: ")
		if !ok {
			continue
		}
		events = append(events, data)
		if data == "[DONE]" {
			return events, nil
		}
	}
	if sc.Err() == nil {
		return events, io.EOF
	}
	return events, sc.Err()
}

func TestStream(t *testing.T) {
	_, url := start(t, func(c *fakeupstream.Config) {
		c.Reply = "番茄炒蛋"
		c.Chunks = 3
	})
	resp, err := do(t, context.Background(), "POST", url+"/v1/chat/completions",
		`{"model":"m1","stream":true,"stream_options":{"include_usage":true},"messages":[{"content":"hi"}]}`)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Errorf("Content-Type = %q", ct)
	}
	events, err := readStream(resp.Body)
	if err != nil {
		t.Fatalf("stream ended with %v after %q", err, events)
	}
	// role, 3 content chunks, finish, usage, [DONE]
	if len(events) != 7 {
		t.Fatalf("got %d events: %q", len(events), events)
	}
	var content []string
	for i, e := range events[:6] {
		c := decode[completion](t, []byte(e))
		if c.Object != "chat.completion.chunk" || c.Model != "m1" {
			t.Errorf("event %d: object, model = %q, %q", i, c.Object, c.Model)
		}
		switch {
		case i == 0:
			if c.Choices[0].Delta.Role != "assistant" {
				t.Errorf("first event is not the role chunk: %s", e)
			}
		case i <= 3:
			content = append(content, c.Choices[0].Delta.Content)
		case i == 4:
			if fr := c.Choices[0].FinishReason; fr == nil || *fr != "stop" {
				t.Errorf("event 4 has no finish_reason: %s", e)
			}
		case i == 5:
			if c.Choices == nil || len(c.Choices) != 0 || c.Usage == nil || c.Usage.CompletionTokens != 3 {
				t.Errorf("event 5 is not the usage chunk: %s", e)
			}
		}
	}
	// Split by character, so no chunk holds half of a multi-byte character.
	if want := []string{"番", "茄", "炒蛋"}; !slices.Equal(content, want) {
		t.Errorf("content chunks = %q, want %q", content, want)
	}
}

func TestStreamWithoutUsage(t *testing.T) {
	_, url := start(t, nil)
	resp, err := do(t, context.Background(), "POST", url+"/v1/chat/completions",
		`{"stream":true,"messages":[{"content":"hi"}]}`)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	events, err := readStream(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range events {
		if strings.Contains(e, `"usage"`) {
			t.Errorf("usage sent without include_usage: %s", e)
		}
	}
}

func TestStreamArrivesChunkByChunk(t *testing.T) {
	const interval = 100 * time.Millisecond
	_, url := start(t, func(c *fakeupstream.Config) {
		c.Chunks = 3
		c.ChunkInterval = interval
	})
	resp, err := do(t, context.Background(), "POST", url+"/v1/chat/completions",
		`{"stream":true,"messages":[{"content":"hi"}]}`)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	began := time.Now()
	var arrived []time.Duration
	sc := bufio.NewScanner(resp.Body)
	for sc.Scan() {
		if strings.HasPrefix(sc.Text(), "data: ") {
			arrived = append(arrived, time.Since(began))
		}
	}
	// role, 3 content chunks, finish, [DONE]
	if len(arrived) != 6 {
		t.Fatalf("got %d events", len(arrived))
	}
	if arrived[0] > interval/2 {
		t.Errorf("role chunk arrived after %v; it should not wait", arrived[0])
	}
	for i := 1; i <= 3; i++ {
		if gap := arrived[i] - arrived[i-1]; gap < interval*8/10 {
			t.Errorf("content chunk %d came %v after the one before; want about %v", i, gap, interval)
		}
	}
}

func TestEmbeddings(t *testing.T) {
	fake, url := start(t, func(c *fakeupstream.Config) { c.Dims = 16 })
	type list struct {
		Object string `json:"object"`
		Model  string `json:"model"`
		Data   []struct {
			Index     int       `json:"index"`
			Embedding []float64 `json:"embedding"`
		} `json:"data"`
		Usage struct {
			PromptTokens int `json:"prompt_tokens"`
			TotalTokens  int `json:"total_tokens"`
		} `json:"usage"`
	}
	status, _, body := call(t, "POST", url+"/v1/embeddings", `{"model":"e","input":["红烧茄子","番茄炒蛋"]}`)
	if status != 200 {
		t.Fatalf("status %d: %s", status, body)
	}
	two := decode[list](t, body)
	if two.Object != "list" || two.Model != "e" || len(two.Data) != 2 {
		t.Fatalf("got %s", body)
	}
	if two.Usage.PromptTokens != 8 || two.Usage.TotalTokens != 8 {
		t.Errorf("usage = %+v, want 8 tokens", two.Usage)
	}
	for i, d := range two.Data {
		if d.Index != i || len(d.Embedding) != 16 {
			t.Errorf("data[%d]: index %d, %d dims", i, d.Index, len(d.Embedding))
		}
		norm := 0.0
		for _, x := range d.Embedding {
			norm += x * x
		}
		if math.Abs(math.Sqrt(norm)-1) > 1e-5 {
			t.Errorf("data[%d] has length %v, want 1", i, math.Sqrt(norm))
		}
	}
	if slices.Equal(two.Data[0].Embedding, two.Data[1].Embedding) {
		t.Error("different inputs got the same vector")
	}

	// A single string gives the same vector as the same text in a batch.
	_, _, body = call(t, "POST", url+"/v1/embeddings", `{"model":"e","input":"番茄炒蛋"}`)
	one := decode[list](t, body)
	if len(one.Data) != 1 || !slices.Equal(one.Data[0].Embedding, two.Data[1].Embedding) {
		t.Errorf("same text, different vector: %v vs %v", one.Data, two.Data[1].Embedding)
	}
	if s := fake.Stats(); s.Requests != 2 || s.PromptTokens != 12 || s.CompletionTokens != 0 {
		t.Errorf("stats = %+v", s)
	}
}

func TestBadRequests(t *testing.T) {
	_, url := start(t, nil)
	tests := []struct {
		name, method, path, body string
		status                   int
		errType                  string
	}{
		{"chat, broken JSON", "POST", "/v1/chat/completions", `{"messages":`, 400, "invalid_request_error"},
		{"chat, no messages", "POST", "/v1/chat/completions", `{"model":"m"}`, 400, "invalid_request_error"},
		{"chat, stream is not a bool", "POST", "/v1/chat/completions", `{"stream":"yes","messages":[{}]}`, 400, "invalid_request_error"},
		{"embeddings, no input", "POST", "/v1/embeddings", `{"model":"e"}`, 400, "invalid_request_error"},
		{"embeddings, null input", "POST", "/v1/embeddings", `{"input":null}`, 400, "invalid_request_error"},
		{"embeddings, empty batch", "POST", "/v1/embeddings", `{"input":[]}`, 400, "invalid_request_error"},
		{"embeddings, number", "POST", "/v1/embeddings", `{"input":42}`, 400, "invalid_request_error"},
		{"unknown path", "POST", "/v1/completions", `{}`, 404, "not_found_error"},
		{"wrong method", "GET", "/v1/embeddings", ``, 404, "not_found_error"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			status, header, body := call(t, tt.method, url+tt.path, tt.body)
			if status != tt.status {
				t.Fatalf("status = %d, want %d: %s", status, tt.status, body)
			}
			if ct := header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
				t.Errorf("Content-Type = %q", ct)
			}
			e := decode[errorBody](t, body)
			if e.Error.Code != tt.status || e.Error.Type != tt.errType || e.Error.Message == "" {
				t.Errorf("error = %+v", e.Error)
			}
		})
	}
}

func TestFailFirst(t *testing.T) {
	fake, url := start(t, func(c *fakeupstream.Config) {
		c.FailFirst = 2
		c.RetryAfter = 3
	})
	statuses := func() []int {
		var got []int
		for range 3 {
			status, header, body := call(t, "POST", url+"/v1/chat/completions", chatBody)
			got = append(got, status)
			if status == 503 {
				if ra := header.Get("Retry-After"); ra != "3" {
					t.Errorf("Retry-After = %q, want 3", ra)
				}
				if e := decode[errorBody](t, body); e.Error.Type != "unavailable_error" {
					t.Errorf("error type = %q", e.Error.Type)
				}
			}
		}
		return got
	}
	if got := statuses(); !slices.Equal(got, []int{503, 503, 200}) {
		t.Errorf("statuses = %v, want [503 503 200]", got)
	}
	if s := fake.Stats(); s.Requests != 3 || s.InjectedFailures != 2 {
		t.Errorf("stats = %+v", s)
	}
	fake.Reset()
	if got := statuses(); !slices.Equal(got, []int{503, 503, 200}) {
		t.Errorf("after reset, statuses = %v, want [503 503 200]", got)
	}
}

func TestFailRateRepeatsAfterReset(t *testing.T) {
	fake, url := start(t, func(c *fakeupstream.Config) {
		c.FailRate = 0.5
		c.FailStatus = 500
		c.Seed = 42
	})
	run := func() []int {
		var got []int
		for range 20 {
			status, header, _ := call(t, "POST", url+"/v1/embeddings", `{"input":"x"}`)
			if status == 500 && header.Get("Retry-After") != "" {
				t.Error("Retry-After sent without retry_after")
			}
			got = append(got, status)
		}
		return got
	}
	first := run()
	if !slices.Contains(first, 200) || !slices.Contains(first, 500) {
		t.Fatalf("fail_rate 0.5 gave %v", first)
	}
	fake.Reset()
	if again := run(); !slices.Equal(again, first) {
		t.Errorf("same seed, different sequence:\n%v\n%v", first, again)
	}
}

func TestDelay(t *testing.T) {
	fake, url := start(t, func(c *fakeupstream.Config) { c.Delay = 200 * time.Millisecond })
	began := time.Now()
	if status, _, _ := call(t, "POST", url+"/v1/embeddings", `{"input":"x"}`); status != 200 {
		t.Fatalf("status %d", status)
	}
	if took := time.Since(began); took < 200*time.Millisecond {
		t.Errorf("answered after %v, before the 200ms delay", took)
	}

	// A caller that gives up during the delay frees its slot.
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := do(t, ctx, "POST", url+"/v1/embeddings", `{"input":"x"}`); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want a deadline error", err)
	}
	waitFor(t, "concurrency to return to 0", func() bool { return fake.Stats().Concurrent == 0 })
}

func TestNonStreamWaitsForWholeReply(t *testing.T) {
	_, url := start(t, func(c *fakeupstream.Config) {
		c.Chunks = 4
		c.ChunkInterval = 50 * time.Millisecond
	})
	began := time.Now()
	call(t, "POST", url+"/v1/chat/completions", chatBody)
	if took := time.Since(began); took < 200*time.Millisecond {
		t.Errorf("answered after %v; 4 chunks 50ms apart take 200ms", took)
	}
}

func TestHang(t *testing.T) {
	fake, url := start(t, func(c *fakeupstream.Config) { c.Hang = true })
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if _, err := do(t, ctx, "POST", url+"/v1/chat/completions", chatBody); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want a deadline error", err)
	}
	waitFor(t, "concurrency to return to 0", func() bool { return fake.Stats().Concurrent == 0 })

	// Close releases a request whose caller would wait forever.
	done := make(chan error, 1)
	go func() {
		resp, err := http.Post(url+"/v1/chat/completions", "application/json", strings.NewReader(chatBody))
		if err == nil {
			resp.Body.Close()
		}
		done <- err
	}()
	waitFor(t, "the request to arrive", func() bool { return fake.Stats().Concurrent == 1 })
	fake.Close()
	select {
	case err := <-done:
		if err == nil {
			t.Error("a hung request got a response")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Close did not release the hung request")
	}
}

func TestBreakAfter(t *testing.T) {
	_, url := start(t, func(c *fakeupstream.Config) { c.BreakAfter = 2 })

	resp, err := do(t, context.Background(), "POST", url+"/v1/chat/completions",
		`{"stream":true,"messages":[{"content":"hi"}]}`)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	events, err := readStream(resp.Body)
	if !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Errorf("stream ended with %v, want unexpected EOF", err)
	}
	// The role chunk and two content chunks, then nothing.
	if len(events) != 3 {
		t.Errorf("got %d events before the cut: %q", len(events), events)
	}

	resp, err = do(t, context.Background(), "POST", url+"/v1/chat/completions", chatBody)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Errorf("status %d", resp.StatusCode)
	}
	if _, err := io.ReadAll(resp.Body); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Errorf("non-streamed body ended with %v, want unexpected EOF", err)
	}
}

func TestBreakAfterLastChunk(t *testing.T) {
	// Breaking after the last content chunk still loses the finish chunk and
	// [DONE], so the caller cannot mistake it for a complete reply.
	_, url := start(t, func(c *fakeupstream.Config) { c.Chunks, c.BreakAfter = 2, 2 })
	resp, err := do(t, context.Background(), "POST", url+"/v1/chat/completions",
		`{"stream":true,"messages":[{"content":"hi"}]}`)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if events, err := readStream(resp.Body); !errors.Is(err, io.ErrUnexpectedEOF) || len(events) != 3 {
		t.Errorf("got %v after %q", err, events)
	}
}

func TestStats(t *testing.T) {
	fake, url := start(t, func(c *fakeupstream.Config) { c.Delay = 200 * time.Millisecond })
	var wg sync.WaitGroup
	for range 5 {
		wg.Go(func() {
			// Not call(): t.Fatal must not run outside the test's goroutine.
			resp, err := http.Post(url+"/v1/embeddings", "application/json", strings.NewReader(`{"input":"x"}`))
			if err != nil {
				t.Error(err)
				return
			}
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
		})
	}
	wg.Wait()

	_, _, body := call(t, "GET", url+"/_stats", "")
	s := decode[fakeupstream.Stats](t, body)
	if s.Requests != 5 || s.MaxConcurrent != 5 || s.Concurrent != 0 {
		t.Errorf("stats = %+v, want 5 requests, max 5 at once, none now", s)
	}
	_, _, body = call(t, "POST", url+"/_reset", "")
	if s := decode[fakeupstream.Stats](t, body); s != (fakeupstream.Stats{}) {
		t.Errorf("after reset, stats = %+v", s)
	}
	// Control endpoints are not model requests.
	if s := fake.Stats(); s.Requests != 0 {
		t.Errorf("control requests were counted: %+v", s)
	}
}

func TestConfigEndpoint(t *testing.T) {
	// Not the default chunk count, to tell a merge into the current config
	// from a merge into the defaults.
	fake, url := start(t, func(c *fakeupstream.Config) { c.Chunks = 7 })

	_, _, body := call(t, "GET", url+"/_config", "")
	if !strings.Contains(string(body), `"chunk_interval":"0s"`) {
		t.Errorf("durations should be strings: %s", body)
	}

	status, _, body := call(t, "POST", url+"/_config", `{"fail_rate":1,"fail_status":429,"retry_after":1,"delay":"5ms"}`)
	if status != 200 {
		t.Fatalf("status %d: %s", status, body)
	}
	got := fake.Config()
	if got.FailRate != 1 || got.FailStatus != 429 || got.Delay != 5*time.Millisecond || got.Chunks != 7 {
		t.Errorf("config after merge = %+v", got)
	}
	status, header, body := call(t, "POST", url+"/v1/chat/completions", chatBody)
	if status != 429 || header.Get("Retry-After") != "1" || decode[errorBody](t, body).Error.Type != "rate_limit_error" {
		t.Errorf("got %d %q %s", status, header.Get("Retry-After"), body)
	}

	for _, bad := range []string{
		`{"fail_rate":2}`,
		`{"fail_rat":1}`,
		`{"delay":5}`,
		`{"delay":"soon"}`,
		`{"chunks":0}`,
		`{"fail_status":200}`,
	} {
		status, _, body := call(t, "POST", url+"/_config", bad)
		if status != 400 || decode[errorBody](t, body).Error.Message == "" {
			t.Errorf("%s: status %d: %s", bad, status, body)
		}
	}
	if after := fake.Config(); after != got {
		t.Errorf("a rejected update changed the config to %+v", after)
	}
}

func TestLast(t *testing.T) {
	fake, url := start(t, nil)
	req, _ := http.NewRequest("POST", url+"/v1/chat/completions",
		strings.NewReader(`{"messages":[{"content":"hi"}],"made_up_field":7}`))
	req.Header.Set("Authorization", "Bearer upstream-key")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	last := fake.Last()
	if last.Method != "POST" || last.Path != "/v1/chat/completions" || last.Header.Get("Authorization") != "Bearer upstream-key" {
		t.Errorf("last = %+v", last)
	}
	_, _, body := call(t, "GET", url+"/_last", "")
	var shown struct {
		Header http.Header    `json:"header"`
		Body   map[string]any `json:"body"`
	}
	if err := json.Unmarshal(body, &shown); err != nil {
		t.Fatalf("%v: %s", err, body)
	}
	if shown.Body["made_up_field"] != 7.0 || shown.Header.Get("Authorization") == "" {
		t.Errorf("/_last = %s", body)
	}
}

func TestLoadTime(t *testing.T) {
	_, url := start(t, func(c *fakeupstream.Config) { c.LoadTime = 300 * time.Millisecond })
	for _, path := range []string{"/health", "/v1/chat/completions"} {
		method := "GET"
		if path != "/health" {
			method = "POST"
		}
		status, _, body := call(t, method, url+path, chatBody)
		if status != 503 || decode[errorBody](t, body).Error.Message != "Loading model" {
			t.Errorf("%s while loading: %d %s", path, status, body)
		}
	}
	waitFor(t, "the model to load", func() bool {
		status, _, _ := call(t, "GET", url+"/health", "")
		return status == 200
	})
	if status, _, _ := call(t, "POST", url+"/v1/chat/completions", chatBody); status != 200 {
		t.Errorf("after loading: status %d", status)
	}
}

func TestNewRejectsBadConfig(t *testing.T) {
	cfg := fakeupstream.DefaultConfig()
	cfg.Chunks = 0
	cfg.FailRate = math.NaN()
	_, err := fakeupstream.New(cfg)
	if err == nil || !strings.Contains(err.Error(), "chunks") || !strings.Contains(err.Error(), "fail_rate") {
		t.Errorf("err = %v, want both problems named", err)
	}
}
