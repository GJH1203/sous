// Package app is the cooking log: it understands the messages, keeps the
// log, and answers. It reaches models only through the gateway, by their
// logical names.
package app

import (
	"context"
	"log/slog"
	"time"

	"github.com/GJH1203/sous/app/bot"
	"github.com/GJH1203/sous/app/llm"
	"github.com/GJH1203/sous/app/store"
)

// App answers the messages that the bot hands it.
type App struct {
	store *store.Store
	llm   *llm.Client
	loc   *time.Location
	log   *slog.Logger
}

// New returns an app that keeps its log in st and calls models with client.
func New(st *store.Store, client *llm.Client, loc *time.Location, log *slog.Logger) *App {
	return &App{store: st, llm: client, loc: loc, log: log}
}

// Handle answers one message. For now the answer is always the same.
func (a *App) Handle(ctx context.Context, m bot.Message) string {
	return "收到。我现在只会回这一句，记菜的功能马上就来。"
}
