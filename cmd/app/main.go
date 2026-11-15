// Command app runs the cooking log's Discord bot. Its config comes from the
// environment; see .env.example.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"
	_ "time/tzdata" // the zone database, for containers that have none

	"github.com/GJH1203/sous/app"
	"github.com/GJH1203/sous/app/bot"
	"github.com/GJH1203/sous/app/llm"
	"github.com/GJH1203/sous/app/store"
)

// grace is how long a shutdown waits for replies in progress. An answer from
// a model can take a while; a reply cut off is a message the user sends again.
const grace = 30 * time.Second

func main() {
	if err := run(); err != nil {
		// Plain text: these are problems for a person to fix, such as a
		// variable that is not set.
		fmt.Fprintf(os.Stderr, "app: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := app.ConfigFromEnv(os.Getenv)
	if err != nil {
		return err
	}
	logger := slog.New(slog.NewJSONHandler(os.Stderr, nil))

	// The first SIGINT or SIGTERM starts a graceful shutdown; a second one
	// ends the process at once.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	context.AfterFunc(ctx, stop)

	st, err := store.Open(ctx, cfg.DBPath)
	if err != nil {
		return err
	}
	defer st.Close()

	client := llm.New(cfg.GatewayURL, cfg.AppKey)
	// Only a warning: the gateway may well start after the app.
	readyCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	if err := client.Ready(readyCtx); err != nil {
		logger.Warn("the gateway is not ready", "url", cfg.GatewayURL, "err", err)
	}
	cancel()

	logger.Info("app starting", "gateway", cfg.GatewayURL, "db", cfg.DBPath,
		"timezone", cfg.Location.String(), "users", len(cfg.AllowedUsers))
	a := app.New(st, client, cfg.Location, logger)
	if err := bot.New(cfg.DiscordToken, cfg.AllowedUsers, a.Handle, logger).Run(ctx, grace); err != nil {
		return err
	}
	logger.Info("app stopped")
	return nil
}
