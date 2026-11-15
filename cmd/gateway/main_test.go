package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/GJH1203/sous/internal/fakeupstream"
)

// gatewayBin is the gateway, built once for all the tests: signals go to a
// process, so these tests run it as one.
var gatewayBin string

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "gateway-test")
	if err != nil {
		panic(err)
	}
	code := 1
	gatewayBin = filepath.Join(dir, "gateway")
	if out, err := exec.Command("go", "build", "-o", gatewayBin, ".").CombinedOutput(); err != nil {
		fmt.Fprintf(os.Stderr, "building the gateway: %v\n%s", err, out)
	} else {
		code = m.Run()
	}
	os.RemoveAll(dir)
	os.Exit(code)
}

const testKey = "test-gateway-key"

// process is the gateway running as a program of its own.
type process struct {
	cmd    *exec.Cmd
	lines  chan string   // its stderr, line by line; closed when it exits
	exited chan struct{} // closed once it has exited
	err    error         // what Wait returned; read it once exited is closed
}

// start runs the gateway with a config that listens on a free port, unless
// listen says otherwise.
func start(t *testing.T, upstream, grace, listen string) *process {
	t.Helper()
	if listen == "" {
		listen = "127.0.0.1:0"
	}
	config := filepath.Join(t.TempDir(), "gateway.yaml")
	text := fmt.Sprintf(`listen: %s
shutdown_grace: %s
upstreams:
  - name: fake
    base_url: %s
models:
  chat: {upstream: fake, model: chat}
callers:
  - name: test
    api_key_env: TEST_GATEWAY_KEY
`, listen, grace, upstream)
	if err := os.WriteFile(config, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(gatewayBin, "-config", config)
	cmd.Env = append(os.Environ(), "TEST_GATEWAY_KEY="+testKey)
	// A pipe of our own rather than StderrPipe, whose reads must all be done
	// before Wait: here Wait runs while the lines are still being read.
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	cmd.Stderr = w
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	w.Close() // the gateway has its own copy; EOF comes when it exits
	p := &process{cmd: cmd, lines: make(chan string, 100), exited: make(chan struct{})}
	go func() {
		sc := bufio.NewScanner(r)
		for sc.Scan() {
			p.lines <- sc.Text()
		}
		close(p.lines)
		r.Close()
	}()
	go func() {
		p.err = cmd.Wait()
		close(p.exited)
	}()
	t.Cleanup(func() {
		cmd.Process.Kill()
		<-p.exited
	})
	return p
}

// waitLog reads the log until a JSON line with the given message, and
// returns it. The lines before it go to the test's output.
func (p *process) waitLog(t *testing.T, msg string) map[string]any {
	t.Helper()
	timeout := time.After(5 * time.Second)
	for {
		select {
		case text, ok := <-p.lines:
			if !ok {
				t.Fatalf("the gateway exited without logging %q", msg)
			}
			t.Log(text)
			var line map[string]any
			if json.Unmarshal([]byte(text), &line) == nil && line["msg"] == msg {
				return line
			}
		case <-timeout:
			t.Fatalf("no %q in the log", msg)
		}
	}
}

// rest returns the log lines left once the gateway has exited.
func (p *process) rest(t *testing.T) string {
	t.Helper()
	var b strings.Builder
	for text := range p.lines {
		t.Log(text)
		b.WriteString(text + "\n")
	}
	return b.String()
}

func (p *process) url(t *testing.T) string {
	t.Helper()
	return "http://" + p.waitLog(t, "gateway listening")["addr"].(string)
}

// wait waits for the gateway to exit, and returns what Wait returned.
func (p *process) wait(t *testing.T, within time.Duration) error {
	t.Helper()
	select {
	case <-p.exited:
		return p.err
	case <-time.After(within):
		t.Fatalf("the gateway has not exited after %v", within)
		return nil
	}
}

func (p *process) signal(t *testing.T, sig os.Signal) {
	t.Helper()
	if err := p.cmd.Process.Signal(sig); err != nil {
		t.Fatal(err)
	}
}

// fakeUpstream streams chunks content chunks, interval apart.
func fakeUpstream(t *testing.T, chunks int, interval time.Duration) string {
	t.Helper()
	cfg := fakeupstream.DefaultConfig()
	cfg.Chunks, cfg.ChunkInterval = chunks, interval
	fake, err := fakeupstream.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(fake)
	t.Cleanup(func() {
		fake.Close()
		srv.Close()
	})
	return srv.URL
}

// stream starts a streaming chat and returns once the first event is in.
func stream(t *testing.T, url string) *bufio.Reader {
	t.Helper()
	req, err := http.NewRequest("POST", url+"/v1/chat/completions",
		strings.NewReader(`{"model":"chat","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+testKey)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	body := bufio.NewReader(resp.Body)
	if _, err := body.ReadString('\n'); err != nil {
		t.Fatalf("no first event: %v", err)
	}
	return body
}

func TestSIGTERMLetsAStreamFinish(t *testing.T) {
	p := start(t, fakeUpstream(t, 5, 200*time.Millisecond), "10s", "")
	body := stream(t, p.url(t))
	p.signal(t, syscall.SIGTERM)
	line := p.waitLog(t, "shutting down")
	if line["cause"] != "terminated signal received" || line["in_flight"] != 1.0 {
		t.Errorf("shutdown line: %v", line)
	}
	rest, err := io.ReadAll(body)
	if err != nil || !strings.HasSuffix(string(rest), "data: [DONE]\n\n") {
		t.Errorf("the rest of the stream: %v\n%s", err, rest)
	}
	if err := p.wait(t, 3*time.Second); err != nil {
		t.Errorf("exit: %v, want 0", err)
	}
	if log := p.rest(t); !strings.Contains(log, `"msg":"gateway stopped"`) || strings.Contains(log, "cut_off") {
		t.Errorf("want a clean stop")
	}
}

// A person who does not want to wait presses Ctrl+C again.
func TestSecondSignalEndsAtOnce(t *testing.T) {
	p := start(t, fakeUpstream(t, 100, 100*time.Millisecond), "1m", "")
	stream(t, p.url(t))
	p.signal(t, syscall.SIGTERM)
	p.waitLog(t, "shutting down")
	// The default comes back just after the first signal; a second one that
	// arrives before then is caught and ignored. So keep sending.
	deadline := time.After(3 * time.Second)
	for {
		p.signal(t, syscall.SIGTERM)
		select {
		case <-p.exited:
			var exit *exec.ExitError
			if !errors.As(p.err, &exit) || exit.Sys().(syscall.WaitStatus).Signal() != syscall.SIGTERM {
				t.Errorf("exit: %v, want killed by SIGTERM", p.err)
			}
			return
		case <-time.After(100 * time.Millisecond):
		case <-deadline:
			t.Fatal("still running; waiting out the grace period?")
		}
	}
}

func TestPortInUse(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	p := start(t, "http://127.0.0.1:1", "1s", ln.Addr().String())
	err = p.wait(t, 5*time.Second)
	if exit, ok := err.(*exec.ExitError); !ok || exit.ExitCode() != 1 {
		t.Errorf("exit: %v, want status 1", err)
	}
	// Plain text, like a bad config, and nothing about listening.
	want := "gateway: listen tcp " + ln.Addr().String() + ": bind: address already in use\n"
	if log := p.rest(t); log != want {
		t.Errorf("stderr:\n%s\nwant\n%s", log, want)
	}
}
