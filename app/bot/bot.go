// Package bot connects the app to Discord. It hands each message from an
// allowed user to a handler, and posts the handler's reply in the same
// channel. Everyone else is ignored.
package bot

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/bwmarrin/discordgo"
)

// Message is a Discord message, cut down to what the app needs.
type Message struct {
	AuthorID  string
	ChannelID string
	Content   string
}

// Handler answers a message. An empty reply posts nothing.
type Handler func(ctx context.Context, m Message) string

// Bot is a Discord bot for a fixed list of users.
type Bot struct {
	token   string
	allowed map[string]bool
	handle  Handler
	log     *slog.Logger
	timeout time.Duration // for handling one message

	mu       sync.Mutex
	stopping bool           // no new messages once set
	running  sync.WaitGroup // messages being handled
	empty    sync.Once      // the warning about messages without content
}

// New returns a bot that logs in with token and answers the users whose IDs
// are in allowed.
func New(token string, allowed []string, handle Handler, log *slog.Logger) *Bot {
	b := &Bot{
		token:   token,
		allowed: make(map[string]bool),
		handle:  handle,
		log:     log,
		// Longer than the llm client's deadline for one answer, so that a
		// slow model ends with the gateway's error rather than this one.
		timeout: 5 * time.Minute,
	}
	for _, id := range allowed {
		b.allowed[id] = true
	}
	return b
}

// Run connects to Discord and answers messages until ctx is done. Then it
// disconnects, and waits up to grace for the messages being handled; those
// still running after that have their contexts cancelled.
func (b *Bot) Run(ctx context.Context, grace time.Duration) error {
	// discordgo logs with the log package on its own; this sends it to the
	// app's log instead. Its info and debug levels, which can hold whole
	// payloads, stay off.
	discordgo.Logger = func(level, _ int, format string, a ...any) {
		lv := slog.LevelWarn
		if level == discordgo.LogError {
			lv = slog.LevelError
		}
		b.log.Log(context.Background(), lv, "discordgo: "+fmt.Sprintf(format, a...))
	}
	s, err := discordgo.New("Bot " + b.token)
	if err != nil {
		return err
	}
	s.LogLevel = discordgo.LogWarning
	// Message content is a privileged intent: it must also be switched on
	// for the bot in the developer portal, or messages in servers arrive
	// without their text.
	s.Identify.Intents = discordgo.IntentsGuildMessages | discordgo.IntentsDirectMessages | discordgo.IntentsMessageContent

	// Not ctx: a reply that is being worked on when the shutdown starts
	// should still go out.
	handling, cancel := context.WithCancel(context.WithoutCancel(ctx))
	defer cancel()
	s.AddHandler(func(_ *discordgo.Session, r *discordgo.Ready) {
		b.log.Info("connected to Discord", "as", r.User.Username)
	})
	// discordgo runs each handler call in a goroutine of its own, so a slow
	// reply does not hold up the others.
	s.AddHandler(func(s *discordgo.Session, m *discordgo.MessageCreate) {
		b.onMessage(handling, s, m)
	})
	if err := s.Open(); err != nil {
		return fmt.Errorf("connecting to Discord: %w", err)
	}
	// Reconnecting after a dropped connection is up to discordgo.
	<-ctx.Done()

	b.log.Info("disconnecting from Discord")
	if err := s.Close(); err != nil {
		b.log.Warn("disconnecting from Discord", "err", err)
	}
	// Replies still go out after Close: they use the REST API, not the
	// connection that brings the messages in.
	if !b.drain(grace) {
		b.log.Warn("grace period over, cancelling replies in progress")
	}
	return nil
}

// sender is the part of *discordgo.Session that posts replies.
type sender interface {
	ChannelMessageSend(channelID, content string, options ...discordgo.RequestOption) (*discordgo.Message, error)
}

func (b *Bot) onMessage(ctx context.Context, s sender, m *discordgo.MessageCreate) {
	// Bots include this one: it sees its own replies.
	if m.Author == nil || m.Author.Bot || !b.allowed[m.Author.ID] {
		return
	}
	if m.Content == "" {
		b.empty.Do(func() {
			b.log.Warn("a message came without its text; is the Message Content intent on?")
		})
		return
	}
	if !b.begin() {
		return
	}
	defer b.running.Done()

	start := time.Now()
	ctx, cancel := context.WithTimeout(ctx, b.timeout)
	defer cancel()
	reply := b.handle(ctx, Message{AuthorID: m.Author.ID, ChannelID: m.ChannelID, Content: m.Content})
	// The IDs and the timing only: what people write stays out of the log.
	b.log.Info("message", "author", m.Author.ID, "channel", m.ChannelID,
		"reply_chars", len([]rune(reply)), "took_ms", time.Since(start).Milliseconds())
	if reply == "" {
		return
	}
	if _, err := s.ChannelMessageSend(m.ChannelID, clip(reply)); err != nil {
		b.log.Error("sending a reply", "channel", m.ChannelID, "err", err)
	}
}

// begin counts a message as being handled, unless the bot is stopping. The
// check and the count happen under one lock, so that drain never misses a
// message that has just come in.
func (b *Bot) begin() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.stopping {
		return false
	}
	b.running.Add(1)
	return true
}

// drain stops taking messages and waits up to grace for those being
// handled. It reports whether they all finished.
func (b *Bot) drain(grace time.Duration) bool {
	b.mu.Lock()
	b.stopping = true
	b.mu.Unlock()
	done := make(chan struct{})
	go func() {
		b.running.Wait()
		close(done)
	}()
	select {
	case <-done:
		return true
	case <-time.After(grace):
		return false
	}
}

// maxReply keeps a reply under Discord's limit of 2000 characters, with room
// to spare for characters that Discord counts as two.
const maxReply = 1900

func clip(s string) string {
	r := []rune(s)
	if len(r) <= maxReply {
		return s
	}
	return string(r[:maxReply-1]) + "…"
}
