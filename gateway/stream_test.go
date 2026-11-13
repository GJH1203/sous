package gateway

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"testing"
	"testing/iotest"
	"time"

	"github.com/GJH1203/sous/internal/fakeupstream"
)

const chatBody = `{"model":"chat","stream":true,"stream_options":{"include_usage":true},` +
	`"messages":[{"role":"user","content":"番茄炒蛋怎么做"}]}`

// splitBody cuts a relayed stream into its events, without the blank lines.
func splitBody(body []byte) []string {
	return strings.Split(strings.TrimSuffix(string(body), "\n\n"), "\n\n")
}

// replyStamp matches the two fields that differ between any two replies.
var replyStamp = regexp.MustCompile(`"(id|created)":("[^"]*"|\d+)`)

func TestStreamSameAsDirect(t *testing.T) {
	r := setup(t, options{})
	direct, directBody := post(t, r.fakeURL+"/v1/chat/completions", nil, chatBody)
	via, viaBody := post(t, r.gatewayURL+"/v1/chat/completions", nil, chatBody)
	directBody = replyStamp.ReplaceAll(directBody, []byte(`"$1":_`))
	viaBody = replyStamp.ReplaceAll(viaBody, []byte(`"$1":_`))
	if via.StatusCode != 200 || string(viaBody) != string(directBody) {
		t.Errorf("via the gateway: %d %s\ndirect: %d %s", via.StatusCode, viaBody, direct.StatusCode, directBody)
	}
	// Role, 5 content chunks, finish, usage, [DONE].
	if n := len(splitBody(viaBody)); n != 9 {
		t.Errorf("%d events, want 9", n)
	}
	for _, name := range []string{"Content-Type", "X-Accel-Buffering"} {
		if via.Header.Get(name) != direct.Header.Get(name) {
			t.Errorf("%s: %q via the gateway, %q direct", name, via.Header.Get(name), direct.Header.Get(name))
		}
	}
}

// The upstream holds back its last event until the caller has the first, so
// a gateway that buffers the stream never delivers it.
func TestStreamEventsArriveAtOnce(t *testing.T) {
	release := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// OpenAI adds the charset; a check for the exact string would miss it.
		w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
		io.WriteString(w, "data: first\n\n")
		http.NewResponseController(w).Flush()
		select {
		case <-release:
		case <-r.Context().Done():
			return
		}
		io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer upstream.Close()
	gw, client := startGateway(t, upstream.URL, "", 5*time.Second)
	defer gw.Close()
	defer client.CloseIdleConnections()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "POST", gw.URL+"/v1/chat/completions", strings.NewReader(chatBody))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	br := bufio.NewReader(resp.Body)
	var first string
	for !strings.HasSuffix(first, "\n\n") {
		line, err := br.ReadString('\n')
		if err != nil {
			t.Fatalf("the first event did not arrive while the upstream was still streaming: %v", err)
		}
		first += line
	}
	if first != "data: first\n\n" {
		t.Errorf("first event %q", first)
	}
	close(release)
	if rest, err := io.ReadAll(br); err != nil || string(rest) != "data: [DONE]\n\n" {
		t.Errorf("rest %q, %v", rest, err)
	}
}

func TestStreamBreakEndsWithErrorEvent(t *testing.T) {
	r := setup(t, options{fake: func(c *fakeupstream.Config) { c.BreakAfter = 2 }})
	resp, body := post(t, r.gatewayURL+"/v1/chat/completions", nil, chatBody)
	if resp.StatusCode != 200 {
		t.Fatalf("status %d; it was sent before the break, so it stays 200", resp.StatusCode)
	}
	got := splitBody(body)
	// Role and 2 content chunks, then the gateway's error event, and no [DONE].
	if len(got) != 4 {
		t.Fatalf("%d events, want 4:\n%s", len(got), body)
	}
	for _, ev := range got[:3] {
		var chunk struct{ Object string }
		if err := json.Unmarshal([]byte(strings.TrimPrefix(ev, "data: ")), &chunk); err != nil || chunk.Object != "chat.completion.chunk" {
			t.Errorf("not a chunk: %q", ev)
		}
	}
	data, ok := strings.CutPrefix(got[3], "event: error\ndata: ")
	if e := gatewayError(t, []byte(data)); !ok || e.Code != "upstream_error" || e.Type != "server_error" {
		t.Errorf("last event %q", got[3])
	}
}

// An upstream that breaks in the middle of an event: the partial event must
// not reach the caller, or the error event would be glued onto it.
func TestStreamDropsCutOffEvent(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: one\n\ndata: tw")
		http.NewResponseController(w).Flush()
		panic(http.ErrAbortHandler)
	}))
	defer upstream.Close()
	gw, client := startGateway(t, upstream.URL, "", 5*time.Second)
	defer gw.Close()
	defer client.CloseIdleConnections()

	_, body := post(t, gw.URL+"/v1/chat/completions", nil, chatBody)
	want := "data: one\n\n" + `event: error` + "\n" +
		`data: {"error":{"message":"the upstream stream ended early","type":"server_error","code":"upstream_error"}}` + "\n\n"
	if string(body) != want {
		t.Errorf("got\n%q\nwant\n%q", body, want)
	}
}

func TestSplitEvents(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want []string
	}{
		{"LF", "data: a\n\ndata: b\n\n", []string{"data: a\n\n", "data: b\n\n"}},
		{"CRLF", "data: a\r\n\r\ndata: b\r\n\r\n", []string{"data: a\r\n\r\n", "data: b\r\n\r\n"}},
		{"CRLF first", "data: a\r\n\r\ndata: b\n\n", []string{"data: a\r\n\r\n", "data: b\n\n"}},
		{"several lines", "event: x\ndata: a\ndata: b\n\n", []string{"event: x\ndata: a\ndata: b\n\n"}},
		{"comment", ": ping\n\ndata: a\n\n", []string{": ping\n\n", "data: a\n\n"}},
		{"cut off", "data: a\n\ndata: b\n", []string{"data: a\n\n"}},
	}
	// All at once, so several events sit in the buffer together; and one byte
	// per read, so every event is put together across reads.
	readers := map[string]func(io.Reader) io.Reader{
		"whole":    func(r io.Reader) io.Reader { return r },
		"bytewise": iotest.OneByteReader,
	}
	for _, tt := range tests {
		for how, reader := range readers {
			t.Run(tt.name+"/"+how, func(t *testing.T) {
				sc := bufio.NewScanner(reader(strings.NewReader(tt.in)))
				sc.Split(splitEvents)
				var got []string
				for sc.Scan() {
					got = append(got, sc.Text())
				}
				if err := sc.Err(); err != nil || !slices.Equal(got, tt.want) {
					t.Errorf("got %q, %v; want %q", got, err, tt.want)
				}
			})
		}
	}
}

func TestErrorBeforeStreamIsPlainJSON(t *testing.T) {
	r := setup(t, options{fake: func(c *fakeupstream.Config) {
		c.FailRate = 1
		c.FailStatus = 400
	}})
	direct, directBody := post(t, r.fakeURL+"/v1/chat/completions", nil, chatBody)
	via, viaBody := post(t, r.gatewayURL+"/v1/chat/completions", nil, chatBody)
	if via.StatusCode != 400 || string(viaBody) != string(directBody) {
		t.Errorf("via the gateway: %d %s\ndirect: %d %s", via.StatusCode, viaBody, direct.StatusCode, directBody)
	}
}

// startStream sends a streaming chat request and returns once the first
// event is in, with a function that hangs up.
func startStream(t *testing.T, url string) (hangUp func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	// Give up on a first event that never comes, instead of hanging the test.
	timer := time.AfterFunc(2*time.Second, cancel)
	req, _ := http.NewRequestWithContext(ctx, "POST", url+"/v1/chat/completions", strings.NewReader(chatBody))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := bufio.NewReader(resp.Body).ReadString('\n'); err != nil {
		t.Fatalf("no first event: %v", err)
	}
	timer.Stop()
	return func() {
		cancel()
		resp.Body.Close()
	}
}

func TestStreamCallerLeavingCancelsUpstream(t *testing.T) {
	tests := []struct {
		name     string
		interval time.Duration
	}{
		// The next write to the caller fails.
		{"while events flow", 50 * time.Millisecond},
		// Nothing is written, as while a long prompt is read in; only the
		// request's context tells the gateway that the caller has gone.
		{"while the upstream is silent", time.Minute},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := setup(t, options{fake: func(c *fakeupstream.Config) {
				c.Chunks = 100
				c.ChunkInterval = tt.interval
			}})
			hangUp := startStream(t, r.gatewayURL)
			if n := r.fake.Stats().Concurrent; n != 1 {
				t.Fatalf("upstream has %d requests, want 1", n)
			}
			hangUp()
			waitFor(t, time.Second, "the upstream stream to be cancelled", func() bool { return r.fake.Stats().Concurrent == 0 })
		})
	}
}

func TestStreamsLeaveNoGoroutines(t *testing.T) {
	before := runtime.NumGoroutine()
	t.Run("streams", func(t *testing.T) {
		r := setup(t, options{fake: func(c *fakeupstream.Config) { c.ChunkInterval = 10 * time.Millisecond }})
		post(t, r.gatewayURL+"/v1/chat/completions", nil, chatBody) // to the end
		startStream(t, r.gatewayURL)()                              // abandoned
		r.fake.Update(func(c *fakeupstream.Config) { c.BreakAfter = 2 })
		post(t, r.gatewayURL+"/v1/chat/completions", nil, chatBody) // broken
	})
	// The subtest's cleanup has closed both servers and the gateway's idle
	// connections; these are the test's own.
	http.DefaultClient.CloseIdleConnections()
	waitFor(t, 2*time.Second, "the goroutine count to go back down", func() bool {
		return runtime.NumGoroutine() <= before
	})
}
