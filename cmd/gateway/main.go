// Command gateway runs the LLM gateway.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"syscall"
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
	srv := gateway.NewServer(cfg, gateway.NewHandler(cfg, proxy, logger), logger)

	// Ctrl+C sends SIGINT; systemd and Docker send SIGTERM. Either one starts
	// a graceful shutdown.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	// stop gives the signals back their default, so a second one ends the
	// process at once instead of waiting out the grace period.
	context.AfterFunc(ctx, stop)

	// Listening here rather than in Serve: a port that is taken fails like a
	// bad config, and "listening" is logged only once it is true.
	ln, err := net.Listen("tcp", cfg.Listen)
	if err != nil {
		fmt.Fprintf(os.Stderr, "gateway: %v\n", err)
		os.Exit(1)
	}
	for _, u := range cfg.Upstreams {
		logger.Info("upstream", "name", u.Name, "base_url", u.BaseURL)
	}
	// The address in use, which differs from the config's when its port is 0.
	logger.Info("gateway listening", "addr", ln.Addr().String())
	if err := srv.Serve(ctx, ln); err != nil {
		logger.Error("server stopped", "err", err)
		os.Exit(1)
	}
	logger.Info("gateway stopped")
}
