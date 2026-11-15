package bot

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/bwmarrin/discordgo"
)

// fakeSession records what the bot posts.
type fakeSession struct {
	mu   sync.Mutex
	sent []string // "channel: content"
	err  error
}

func (f *fakeSession) ChannelMessageSend(channelID, content string, _ ...discordgo.RequestOption) (*discordgo.Message, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sent = append(f.sent, channelID+": "+content)
	return &discordgo.Message{}, f.err
}

func message(author string, bot bool, content string) *discordgo.MessageCreate {
	return &discordgo.MessageCreate{Message: &discordgo.Message{
		ChannelID: "c1",
		Content:   content,
		Author:    &discordgo.User{ID: author, Bot: bot},
	}}
}

// testBot answers "echo: " plus the message, and records what it was given.
func testBot(t *testing.T, logs *bytes.Buffer) (*Bot, *[]Message) {
	t.Helper()
	var got []Message
	var mu sync.Mutex
	handle := func(ctx context.Context, m Message) string {
		mu.Lock()
		defer mu.Unlock()
		got = append(got, m)
		if m.Content == "silent" {
			return ""
		}
		return "echo: " + m.Content
	}
	return New("token", []string{"alice", "bob"}, handle, slog.New(slog.NewJSONHandler(logs, nil))), &got
}

func TestOnlyAllowedUsersAreAnswered(t *testing.T) {
	var logs bytes.Buffer
	b, got := testBot(t, &logs)
	s := &fakeSession{}
	for _, m := range []*discordgo.MessageCreate{
		message("alice", false, "今天做了番茄炒蛋"),
		message("mallory", false, "hello"), // not on the list
		message("bob", true, "I am a bot"), // a bot, even with an allowed ID
		message("bob", false, "silent"),    // answered with nothing
		{Message: &discordgo.Message{Content: "no author"}},
	} {
		b.onMessage(context.Background(), s, m)
	}
	if len(*got) != 2 || (*got)[0] != (Message{AuthorID: "alice", ChannelID: "c1", Content: "今天做了番茄炒蛋"}) {
		t.Errorf("handler got %+v", *got)
	}
	if len(s.sent) != 1 || s.sent[0] != "c1: echo: 今天做了番茄炒蛋" {
		t.Errorf("sent %q", s.sent)
	}
	if strings.Contains(logs.String(), "番茄") {
		t.Errorf("a message's text is in the log:\n%s", logs.String())
	}
}

// Without the Message Content intent, messages in servers arrive empty.
func TestEmptyMessagesWarnOnce(t *testing.T) {
	var logs bytes.Buffer
	b, got := testBot(t, &logs)
	s := &fakeSession{}
	for range 3 {
		b.onMessage(context.Background(), s, message("alice", false, ""))
	}
	if len(*got) != 0 || len(s.sent) != 0 {
		t.Errorf("handled %d, sent %d", len(*got), len(s.sent))
	}
	if n := strings.Count(logs.String(), "Message Content intent"); n != 1 {
		t.Errorf("%d warnings, want 1:\n%s", n, logs.String())
	}
}

func TestSendErrorIsLogged(t *testing.T) {
	var logs bytes.Buffer
	b, _ := testBot(t, &logs)
	b.onMessage(context.Background(), &fakeSession{err: errors.New("403 Forbidden")}, message("alice", false, "hi"))
	if !strings.Contains(logs.String(), `"msg":"sending a reply"`) || !strings.Contains(logs.String(), "403 Forbidden") {
		t.Errorf("log:\n%s", logs.String())
	}
}

func TestLongRepliesAreClipped(t *testing.T) {
	b := New("token", []string{"alice"}, func(ctx context.Context, m Message) string {
		return strings.Repeat("菜", 5000)
	}, slog.New(slog.DiscardHandler))
	s := &fakeSession{}
	b.onMessage(context.Background(), s, message("alice", false, "hi"))
	if len(s.sent) != 1 || utf8.RuneCountInString(s.sent[0]) > maxReply+len("c1: ") {
		t.Errorf("sent %d messages, the first %d characters long", len(s.sent), utf8.RuneCountInString(s.sent[0]))
	}

	short := strings.Repeat("菜", maxReply)
	if clip(short) != short {
		t.Error("a reply at the limit was clipped")
	}
	long := clip(strings.Repeat("菜", 5000))
	if n := utf8.RuneCountInString(long); n != maxReply || !strings.HasSuffix(long, "…") {
		t.Errorf("clipped to %d characters, ending %q", n, long[len(long)-3:])
	}
}

func TestHandlerGetsATimeout(t *testing.T) {
	b := New("token", []string{"alice"}, func(ctx context.Context, m Message) string {
		if _, ok := ctx.Deadline(); !ok {
			return "no deadline"
		}
		return "ok"
	}, slog.New(slog.DiscardHandler))
	s := &fakeSession{}
	b.onMessage(context.Background(), s, message("alice", false, "hi"))
	if len(s.sent) != 1 || s.sent[0] != "c1: ok" {
		t.Errorf("sent %q", s.sent)
	}
}

// drain waits for the replies in progress, and refuses new messages.
func TestDrain(t *testing.T) {
	release := make(chan struct{})
	started := make(chan struct{})
	b := New("token", []string{"alice"}, func(ctx context.Context, m Message) string {
		close(started)
		<-release
		return "done"
	}, slog.New(slog.DiscardHandler))
	s := &fakeSession{}
	go b.onMessage(context.Background(), s, message("alice", false, "slow"))
	<-started

	drained := make(chan bool)
	go func() { drained <- b.drain(5 * time.Second) }()
	select {
	case <-drained:
		t.Fatal("drain returned with a reply in progress")
	case <-time.After(100 * time.Millisecond):
	}
	// Arrives while stopping: dropped, and not waited for.
	b.onMessage(context.Background(), s, message("alice", false, "late"))
	close(release)
	if ok := <-drained; !ok {
		t.Error("drain reported unfinished replies")
	}
	if len(s.sent) != 1 || s.sent[0] != "c1: done" {
		t.Errorf("sent %q", s.sent)
	}
}

func TestDrainGivesUp(t *testing.T) {
	started := make(chan struct{})
	b := New("token", []string{"alice"}, func(ctx context.Context, m Message) string {
		close(started)
		<-ctx.Done()
		return ""
	}, slog.New(slog.DiscardHandler))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go b.onMessage(ctx, &fakeSession{}, message("alice", false, "stuck"))
	<-started
	start := time.Now()
	if b.drain(200 * time.Millisecond) {
		t.Error("drain reported every reply finished")
	}
	if took := time.Since(start); took < 200*time.Millisecond || took > time.Second {
		t.Errorf("drain took %v", took)
	}
}
