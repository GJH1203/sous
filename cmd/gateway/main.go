// Command gateway runs the LLM gateway.
package main

import (
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"time"

	"github.com/GJH1203/sous/gateway"
)

func main() {
	configPath := flag.String("config", "config/gateway.yaml", "path to the config file")
	flag.Parse()

	cfg, err := gateway.LoadConfig(*configPath)
	if err != nil {
		// Plain text rather than a JSON log line: a person reads this one and
		// fixes the file.
		fmt.Fprintf(os.Stderr, "gateway: %v\n", err)
		os.Exit(1)
	}

	logger := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	// A non-streaming reply's headers come only once the whole reply is
	// generated, so this is really a limit on generation time.
	client := gateway.NewClient(120 * time.Second)
	proxy := gateway.NewProxy(client, cfg, logger)

	mux := http.NewServeMux()
	mux.Handle("POST /v1/chat/completions", proxy)
	mux.Handle("POST /v1/embeddings", proxy)

	srv := &http.Server{
		Addr:    cfg.Listen,
		Handler: mux,
		// Limits how long a client may take to send its headers. There is no
		// WriteTimeout: it would cut long streams off.
		ReadHeaderTimeout: 10 * time.Second,
	}
	for _, u := range cfg.Upstreams {
		logger.Info("upstream", "name", u.Name, "base_url", u.BaseURL)
	}
	logger.Info("gateway listening", "addr", cfg.Listen)
	if err := srv.ListenAndServe(); err != nil {
		logger.Error("server stopped", "err", err)
		os.Exit(1)
	}
}
