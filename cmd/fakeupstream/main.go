// Command fakeupstream runs a fake OpenAI-compatible model server. Its
// latency and failures are set by flags, and can be changed while it runs
// with POST /_config. The endpoints are listed in internal/fakeupstream.
package main

import (
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"

	"github.com/GJH1203/sous/internal/fakeupstream"
)

func main() {
	cfg := fakeupstream.DefaultConfig()
	addr := flag.String("addr", "127.0.0.1:9001", "address to listen on")
	flag.StringVar(&cfg.Name, "name", cfg.Name, "name returned in the X-Fake-Upstream header")
	flag.IntVar(&cfg.Chunks, "chunks", cfg.Chunks, "content chunks per reply")
	flag.DurationVar(&cfg.ChunkInterval, "chunk-interval", cfg.ChunkInterval, "pause before each content chunk")
	flag.StringVar(&cfg.Reply, "reply", cfg.Reply, `assistant content, split across the chunks (default "w0 w1 ...")`)
	flag.IntVar(&cfg.Dims, "dims", cfg.Dims, "embedding dimensions")
	flag.DurationVar(&cfg.LoadTime, "load-time", cfg.LoadTime, "answer 503 \"Loading model\" for this long after start")
	flag.DurationVar(&cfg.Delay, "delay", cfg.Delay, "delay before the response headers")
	flag.DurationVar(&cfg.PromptDelay, "prompt-delay", cfg.PromptDelay, "time spent reading the prompt: a stream gets its headers before it, its first event after")
	flag.BoolVar(&cfg.Hang, "hang", cfg.Hang, "read requests and never answer")
	flag.IntVar(&cfg.FailFirst, "fail-first", cfg.FailFirst, "fail the first N requests")
	flag.Float64Var(&cfg.FailRate, "fail-rate", cfg.FailRate, "fail each request with this probability")
	flag.IntVar(&cfg.FailStatus, "fail-status", cfg.FailStatus, "status code of an injected failure")
	flag.IntVar(&cfg.RetryAfter, "retry-after", cfg.RetryAfter, "Retry-After seconds on an injected 429 or 503 (0: no header)")
	flag.IntVar(&cfg.BreakAfter, "break-after", cfg.BreakAfter, "cut the connection after N streamed chunks, or halfway through a non-streamed body (0: never)")
	flag.Uint64Var(&cfg.Seed, "seed", cfg.Seed, "random seed for -fail-rate")
	flag.Parse()

	srv, err := fakeupstream.New(cfg)
	if err != nil {
		fmt.Fprintln(os.Stderr, "fakeupstream:", err)
		os.Exit(2)
	}
	// Listen first, so the line below is only printed once the port is ours.
	ln, err := net.Listen("tcp", *addr)
	if err != nil {
		log.Fatal(err)
	}
	log.Printf("fakeupstream %q listening on http://%s", cfg.Name, ln.Addr())
	log.Fatal(http.Serve(ln, srv))
}
