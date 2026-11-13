// Command gateway runs the LLM gateway.
package main

import (
	"flag"
	"log/slog"
	"net/http"
	"os"
	"time"

	"github.com/GJH1203/sous/gateway"
)

func main() {
	listen := flag.String("listen", "127.0.0.1:8080", "address to listen on")
	upstream := flag.String("upstream", "http://127.0.0.1:9001", "base URL of the upstream")
	flag.Parse()

	logger := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	// A non-streaming reply's headers come only once the whole reply is
	// generated, so this is really a limit on generation time.
	client := gateway.NewClient(120 * time.Second)
	proxy := gateway.NewProxy(client, *upstream, os.Getenv("SOUS_UPSTREAM_KEY"), logger)

	mux := http.NewServeMux()
	mux.Handle("POST /v1/chat/completions", proxy)
	mux.Handle("POST /v1/embeddings", proxy)

	srv := &http.Server{
		Addr:    *listen,
		Handler: mux,
		// Limits how long a client may take to send its headers. There is no
		// WriteTimeout: it would cut long streams off.
		ReadHeaderTimeout: 10 * time.Second,
	}
	logger.Info("gateway listening", "addr", *listen, "upstream", *upstream)
	if err := srv.ListenAndServe(); err != nil {
		logger.Error("server stopped", "err", err)
		os.Exit(1)
	}
}
