// Package app is the cooking log: it understands the messages, keeps the
// log, and answers. It reaches models only through the gateway, by their
// logical names.
package app

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/GJH1203/sous/app/bot"
	"github.com/GJH1203/sous/app/extract"
	"github.com/GJH1203/sous/app/store"
)

// App answers the messages that the bot hands it.
type App struct {
	store  *store.Store
	models extract.Chatter
	loc    *time.Location
	log    *slog.Logger
	now    func() time.Time // the clock; tests set their own

	mu    sync.Mutex
	users map[string]*user
}

// New returns an app that keeps its log in st, reaches models through
// models, and takes "today" to be a day in loc.
func New(st *store.Store, models extract.Chatter, loc *time.Location, log *slog.Logger) *App {
	return &App{store: st, models: models, loc: loc, log: log, now: time.Now, users: make(map[string]*user)}
}

// user is what the app keeps about one person between their messages. It
// is only in memory: a restart forgets a record that waits for "对", and
// the person sends it again.
type user struct {
	id string
	// turn holds a token while one of the person's messages is handled.
	// The bot handles each message in a goroutine of its own, so without
	// it a "对" sent while a correction is still with the model would save
	// the record from before the correction.
	turn  chan struct{}
	draft *draft // nil when nothing waits for "对"
}

const help = "发「记 今天做了番茄炒蛋，8 分」记一道菜。"

// Handle answers one message.
func (a *App) Handle(ctx context.Context, m bot.Message) string {
	u, err := a.take(ctx, m.AuthorID)
	if err != nil {
		return "" // the bot is shutting down
	}
	defer func() { <-u.turn }()

	now := a.now().In(a.loc)
	expired := u.draft != nil && now.Sub(u.draft.at) > draftTTL
	if expired {
		u.draft = nil
	}
	text := strings.TrimSpace(m.Content)
	if rest, ok := command(text, "记"); ok {
		return a.record(ctx, u, rest, now)
	}
	return a.answer(ctx, u, text, now, expired)
}

// take waits for the person's turn: until no other message of theirs is
// being handled. Other people do not wait.
func (a *App) take(ctx context.Context, id string) (*user, error) {
	a.mu.Lock()
	u := a.users[id]
	if u == nil {
		// One per person on the bot's list, so the map stays small.
		u = &user{id: id, turn: make(chan struct{}, 1)}
		a.users[id] = u
	}
	a.mu.Unlock()
	select {
	case u.turn <- struct{}{}:
		return u, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// command reports whether text is the command word followed by a space or
// a colon, or by nothing, and returns the rest. "记 番茄炒蛋" and
// "记：番茄炒蛋" are the command 记; "记得买菜" is not.
func command(text, word string) (string, bool) {
	rest, ok := strings.CutPrefix(text, word)
	if !ok {
		return "", false
	}
	if rest == "" {
		return "", true
	}
	r, size := utf8.DecodeRuneInString(rest)
	if !unicode.IsSpace(r) && r != ':' && r != '：' {
		return "", false
	}
	return strings.TrimSpace(rest[size:]), true
}
