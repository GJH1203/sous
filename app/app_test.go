package app

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/GJH1203/sous/app/bot"
	"github.com/GJH1203/sous/app/extract"
	"github.com/GJH1203/sous/app/llm"
	"github.com/GJH1203/sous/app/store"
	"github.com/GJH1203/sous/internal/fakeupstream"
)

// models stands in for the models behind the gateway: it gives its replies
// in order, and keeps every request. With gate set, each call first waits
// for a value on it.
type models struct {
	mu      sync.Mutex
	replies []string
	err     error
	got     []llm.ChatRequest
	gate    chan struct{}
}

func (m *models) Chat(ctx context.Context, feature string, req llm.ChatRequest) (string, error) {
	m.mu.Lock()
	m.got = append(m.got, req)
	n, gate, err := len(m.got), m.gate, m.err
	m.mu.Unlock()
	if gate != nil {
		select {
		case <-gate:
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}
	if err != nil {
		return "", err
	}
	if n > len(m.replies) {
		return "", errors.New("the script has run out of replies")
	}
	return m.replies[n-1], nil
}

func (m *models) calls() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.got)
}

func (m *models) request(i int) llm.ChatRequest {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.got[i]
}

// logBuffer is a log that the app writes from several goroutines.
type logBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *logBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *logBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// tester is an app with scripted models, a database of its own, and a
// clock that stands still until a test moves it.
type tester struct {
	*App
	t      *testing.T
	models *models
	db     string
	logs   *logBuffer
	clock  time.Time
}

// zone is west of UTC, and the clock starts on a Saturday evening there,
// when it is already Sunday in UTC: "today" must be the local day.
var zone = time.FixedZone("UTC-8", -8*3600)

func newTester(t *testing.T, replies ...string) *tester {
	return newTesterWith(t, &models{replies: replies})
}

func newTesterWith(t *testing.T, m extract.Chatter) *tester {
	t.Helper()
	path := filepath.Join(t.TempDir(), "sous.db")
	st, err := store.Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	logs := &logBuffer{}
	tt := &tester{
		App:   New(st, m, zone, slog.New(slog.NewJSONHandler(logs, nil))),
		t:     t,
		db:    path,
		logs:  logs,
		clock: time.Date(2026, 11, 15, 4, 30, 0, 0, time.UTC),
	}
	tt.models, _ = m.(*models)
	tt.App.now = func() time.Time { return tt.clock }
	return tt
}

func (tt *tester) say(author, text string) string {
	tt.t.Helper()
	return tt.Handle(context.Background(), bot.Message{AuthorID: author, ChannelID: "c1", Content: text})
}

// cooks lists what the database holds, one line per cook:
// dish|cooked_on|rating|note|author|source|ingredients.
func (tt *tester) cooks() []string {
	tt.t.Helper()
	db, err := sql.Open("sqlite", "file:"+tt.db)
	if err != nil {
		tt.t.Fatal(err)
	}
	defer db.Close()
	rows, err := db.Query(`
		SELECT d.name, c.cooked_on, c.rating, c.note, c.author_id, c.source,
		       (SELECT group_concat(i.name, ',' ORDER BY i.name)
		          FROM cook_ingredients ci JOIN ingredients i ON i.id = ci.ingredient_id
		         WHERE ci.cook_id = c.id)
		  FROM cooks c JOIN dishes d ON d.id = c.dish_id
		 ORDER BY c.id`)
	if err != nil {
		tt.t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var dish, on, note, author, source string
		var rating sql.NullFloat64
		var ingredients sql.NullString
		if err := rows.Scan(&dish, &on, &rating, &note, &author, &source, &ingredients); err != nil {
			tt.t.Fatal(err)
		}
		r := "-"
		if rating.Valid {
			r = fmt.Sprint(rating.Float64)
		}
		out = append(out, strings.Join([]string{dish, on, r, note, author, source, ingredients.String}, "|"))
	}
	return out
}

func (tt *tester) wantCooks(want ...string) {
	tt.t.Helper()
	if got := tt.cooks(); !slices.Equal(got, want) {
		tt.t.Errorf("the database holds\n%q\nwant\n%q", got, want)
	}
}

func wantReply(t *testing.T, got, want string) {
	t.Helper()
	if got != want {
		t.Errorf("reply\n%s\nwant\n%s", got, want)
	}
}

const (
	yuxiang = `{"dish": "鱼香茄子", "ingredients": ["茄子", "猪肉末"], "rating": 9, "note": "少放了糖", "date": "2026-11-14"}`
	tomato  = `{"dish": "番茄炒蛋", "ingredients": ["番茄", "鸡蛋"], "rating": null, "note": "", "date": "2026-11-13"}`
	mapo    = `{"dish": "麻婆豆腐", "ingredients": ["豆腐", "牛肉末"], "rating": 8.5, "note": "", "date": "2026-11-11"}`
	ask     = "回「对」保存，回「算了」放弃，或者直接说哪里要改。"
	nothing = "现在没有等着确认的记录。"
)

func TestRecordThreeDishes(t *testing.T) {
	tt := newTester(t, yuxiang, tomato, mapo, yuxiang)

	wantReply(t, tt.say("u1", "记 今天做了鱼香茄子，少放了糖，9 分"),
		"记下这条吗？\n菜：鱼香茄子\n食材：茄子、猪肉末\n评分：9 分\n备注：少放了糖\n日期：11 月 14 日 周六（今天）\n"+ask)
	tt.wantCooks() // nothing until it is confirmed
	wantReply(t, tt.say("u1", "对"), "记好了。第一次做鱼香茄子。")

	wantReply(t, tt.say("u1", "记：昨天的番茄炒蛋"),
		"记下这条吗？\n菜：番茄炒蛋\n食材：番茄、鸡蛋\n评分：没打分\n备注：无\n日期：11 月 13 日 周五（昨天）\n"+ask)
	wantReply(t, tt.say("u1", "对的。"), "记好了。第一次做番茄炒蛋。")

	if reply := tt.say("u2", "记 周三做了麻婆豆腐，用的牛肉末，8.5 分"); !strings.Contains(reply, "评分：8.5 分\n") ||
		!strings.Contains(reply, "日期：11 月 11 日 周三\n") {
		t.Errorf("reply\n%s", reply)
	}
	wantReply(t, tt.say("u2", "OK!"), "记好了。第一次做麻婆豆腐。")

	tt.say("u2", "记 鱼香茄子又做了一次")
	wantReply(t, tt.say("u2", "好"), "记好了。这是第 2 次做鱼香茄子。")

	tt.wantCooks(
		"鱼香茄子|2026-11-14|9|少放了糖|u1|今天做了鱼香茄子，少放了糖，9 分|猪肉末,茄子",
		"番茄炒蛋|2026-11-13|-||u1|昨天的番茄炒蛋|番茄,鸡蛋",
		"麻婆豆腐|2026-11-11|8.5||u2|周三做了麻婆豆腐，用的牛肉末，8.5 分|牛肉末,豆腐",
		"鱼香茄子|2026-11-14|9|少放了糖|u2|鱼香茄子又做了一次|猪肉末,茄子",
	)
	// "对" again saves nothing more.
	wantReply(t, tt.say("u2", "对"), nothing)
}

func TestCorrection(t *testing.T) {
	tt := newTester(t,
		`{"dish": "番茄炒蛋", "ingredients": ["番茄", "鸡蛋"], "rating": 7, "note": "", "date": "2026-11-14"}`,
		`{"dish": "番茄炒蛋", "ingredients": ["番茄", "鸡蛋", "木耳"], "rating": 7, "note": "", "date": "2026-11-14"}`,
		`{"dish": "番茄炒蛋", "ingredients": ["番茄", "鸡蛋", "木耳"], "rating": 8, "note": "", "date": "2026-11-14"}`,
	)
	// Ten to midnight; the correction comes after it.
	tt.clock = time.Date(2026, 11, 14, 23, 50, 0, 0, zone)
	tt.say("u1", "记 番茄炒蛋 7 分")
	tt.clock = tt.clock.Add(15 * time.Minute)
	wantReply(t, tt.say("u1", "还放了木耳"),
		"改好了。\n记下这条吗？\n菜：番茄炒蛋\n食材：番茄、鸡蛋、木耳\n评分：7 分\n备注：无\n日期：11 月 14 日 周六（今天）\n"+ask)
	// The model saw the message, as of the day it was sent, its record,
	// and the correction.
	msgs := tt.models.request(1).Messages
	if len(msgs) != 4 || msgs[1].Content != "Today: 2026-11-14, Saturday\nMessage: 番茄炒蛋 7 分" ||
		msgs[3] != (llm.Message{Role: "user", Content: "Correction: 还放了木耳"}) {
		t.Fatalf("the correction went as %+v", msgs)
	}
	// A second correction starts from the first one's record.
	if reply := tt.say("u1", "对，但是 8 分"); !strings.Contains(reply, "评分：8 分") {
		t.Errorf("reply\n%s", reply)
	}
	if msgs := tt.models.request(2).Messages; !strings.Contains(msgs[2].Content, "木耳") {
		t.Errorf("the second correction went as %+v", msgs)
	}
	wantReply(t, tt.say("u1", "对"), "记好了。第一次做番茄炒蛋。")
	tt.wantCooks("番茄炒蛋|2026-11-14|8||u1|番茄炒蛋 7 分\n还放了木耳\n对，但是 8 分|木耳,番茄,鸡蛋")
}

// A correction that the model cannot apply leaves the draft as it was.
func TestFailedCorrectionKeepsTheDraft(t *testing.T) {
	tt := newTester(t, yuxiang, "好的", "好的")
	tt.say("u1", "记 今天做了鱼香茄子，少放了糖，9 分")
	wantReply(t, tt.say("u1", "啊啊啊"), "没看懂这条修改，记录还是上面那样。回「对」保存，回「算了」放弃，或者换个说法再改。")
	tt.say("u1", "对")
	tt.wantCooks("鱼香茄子|2026-11-14|9|少放了糖|u1|今天做了鱼香茄子，少放了糖，9 分|猪肉末,茄子")
}

func TestCancel(t *testing.T) {
	tt := newTester(t, yuxiang)
	tt.say("u1", "记 今天做了鱼香茄子，少放了糖，9 分")
	wantReply(t, tt.say("u1", "算了"), "好，「鱼香茄子」不记了。")
	wantReply(t, tt.say("u1", "对"), nothing)
	tt.wantCooks()
}

// Acceptance: a model that keeps giving broken JSON, through the real
// client and the fake upstream, gets an answer and leaves nothing behind.
func TestBrokenJSONFromTheUpstream(t *testing.T) {
	cfg := fakeupstream.DefaultConfig()
	cfg.ChunkInterval = 0
	cfg.Reply = yuxiang[:50] // cut off halfway
	fake, err := fakeupstream.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(fake)
	defer srv.Close()
	defer fake.Close()

	tt := newTesterWith(t, llm.New(srv.URL, "key"))
	wantReply(t, tt.say("u1", "记 今天做了鱼香茄子，少放了糖，9 分"),
		"没看懂这条记录，没有记下。换个说法再发一次，比如「记 今天做了番茄炒蛋，8 分」。")
	wantReply(t, tt.say("u1", "对"), nothing)
	tt.wantCooks()
	if n := fake.Stats().Requests; n != 2 {
		t.Errorf("%d requests to the model, want 2", n)
	}
}

func TestModelUnreachable(t *testing.T) {
	tt := newTester(t)
	tt.models.err = &llm.Error{Status: 502, Type: "upstream_error", Message: "connection refused"}
	wantReply(t, tt.say("u1", "记 今天做了鱼香茄子"), "现在叫不到模型，这条没有记下。过一会儿再发一次。")
	wantReply(t, tt.say("u1", "对"), nothing)
	if !strings.Contains(tt.logs.String(), "connection refused") {
		t.Errorf("the error is not in the log:\n%s", tt.logs)
	}
}

func TestSaveFailureKeepsTheDraft(t *testing.T) {
	tt := newTester(t, yuxiang)
	tt.say("u1", "记 今天做了鱼香茄子，少放了糖，9 分")
	tt.store.Close()
	wantReply(t, tt.say("u1", "对"), "保存失败了，记录还在。过一会儿再回「对」试试。")
	wantReply(t, tt.say("u1", "算了"), "好，「鱼香茄子」不记了。")
	if !strings.Contains(tt.logs.String(), "saving a cook") {
		t.Errorf("the failure is not in the log:\n%s", tt.logs)
	}
}

// One person's "对" does not save the other's draft.
func TestDraftsArePerUser(t *testing.T) {
	tt := newTester(t, yuxiang)
	tt.say("u1", "记 今天做了鱼香茄子，少放了糖，9 分")
	wantReply(t, tt.say("u2", "对"), nothing)
	wantReply(t, tt.say("u1", "对"), "记好了。第一次做鱼香茄子。")
	tt.wantCooks("鱼香茄子|2026-11-14|9|少放了糖|u1|今天做了鱼香茄子，少放了糖，9 分|猪肉末,茄子")
}

func TestDraftExpires(t *testing.T) {
	tt := newTester(t, yuxiang, tomato)
	tt.say("u1", "记 今天做了鱼香茄子，少放了糖，9 分")
	tt.clock = tt.clock.Add(draftTTL)
	wantReply(t, tt.say("u1", "对"), "记好了。第一次做鱼香茄子。")

	tt.say("u1", "记 昨天的番茄炒蛋")
	tt.clock = tt.clock.Add(draftTTL + time.Second)
	// Not taken as a correction: it is likely about something else.
	wantReply(t, tt.say("u1", "今天好累"), "上一条记录等了太久，已经放弃了。要记的话，重新发「记 …」。")
	wantReply(t, tt.say("u1", "对"), nothing)
	tt.wantCooks("鱼香茄子|2026-11-14|9|少放了糖|u1|今天做了鱼香茄子，少放了糖，9 分|猪肉末,茄子")
	if n := tt.models.calls(); n != 2 {
		t.Errorf("%d model calls, want 2", n)
	}
}

func TestNewRecordDropsTheDraft(t *testing.T) {
	tt := newTester(t, yuxiang, tomato, yuxiang, "坏", "坏")
	tt.say("u1", "记 今天做了鱼香茄子，少放了糖，9 分")
	if reply := tt.say("u1", "记 昨天的番茄炒蛋"); !strings.HasPrefix(reply, "（上一条「鱼香茄子」没保存，已放弃。）\n记下这条吗？\n菜：番茄炒蛋") {
		t.Errorf("reply\n%s", reply)
	}
	tt.say("u1", "对")
	tt.wantCooks("番茄炒蛋|2026-11-13|-||u1|昨天的番茄炒蛋|番茄,鸡蛋")

	// Dropped even when the new one is not understood.
	tt.say("u1", "记 今天做了鱼香茄子，少放了糖，9 分")
	wantReply(t, tt.say("u1", "记 今天天气不错"),
		"（上一条「鱼香茄子」没保存，已放弃。）\n没看懂这条记录，没有记下。换个说法再发一次，比如「记 今天做了番茄炒蛋，8 分」。")
	wantReply(t, tt.say("u1", "对"), nothing)
}

func TestNotUnderstood(t *testing.T) {
	tt := newTester(t, "今天天气不错", `{"dish": ""}`)
	wantReply(t, tt.say("u1", "记 今天天气不错"),
		"没看懂这条记录，没有记下。换个说法再发一次，比如「记 今天做了番茄炒蛋，8 分」。")
	wantReply(t, tt.say("u1", "对"), nothing)
	tt.wantCooks()
}

// Messages that are not commands and answer no draft get the help, without
// a model call.
func TestHelp(t *testing.T) {
	tt := newTester(t)
	for _, text := range []string{"你好", "记得买菜", ""} {
		wantReply(t, tt.say("u1", text), help)
	}
	wantReply(t, tt.say("u1", "记"), "要记什么？比如「记 今天做了番茄炒蛋，8 分」。")
	if n := tt.models.calls(); n != 0 {
		t.Errorf("%d model calls", n)
	}
}

func TestCommand(t *testing.T) {
	tests := []struct {
		text, rest string
		ok         bool
	}{
		{"记 番茄炒蛋", "番茄炒蛋", true},
		{"记  番茄炒蛋 ", "番茄炒蛋", true},
		{"记：番茄炒蛋", "番茄炒蛋", true},
		{"记:番茄炒蛋", "番茄炒蛋", true},
		{"记\u3000番茄炒蛋", "番茄炒蛋", true}, // a full-width space
		{"记\n番茄炒蛋", "番茄炒蛋", true},
		{"记", "", true},
		{"记得买菜", "", false},
		{"番茄炒蛋", "", false},
		{" 记 番茄炒蛋", "", false}, // Handle trims first
	}
	for _, tt := range tests {
		if rest, ok := command(tt.text, "记"); rest != tt.rest || ok != tt.ok {
			t.Errorf("command(%q) = %q, %v; want %q, %v", tt.text, rest, ok, tt.rest, tt.ok)
		}
	}
}

func TestDay(t *testing.T) {
	today := time.Date(2026, 1, 2, 20, 30, 0, 0, zone) // a Friday
	for date, want := range map[string]string{
		"2026-01-02": "1 月 2 日 周五（今天）",
		"2026-01-01": "1 月 1 日 周四（昨天）",
		"2025-12-31": "2025 年 12 月 31 日 周三（前天）",
		"2025-12-30": "2025 年 12 月 30 日 周二",
	} {
		if got := day(date, today); got != want {
			t.Errorf("day(%s) = %q, want %q", date, got, want)
		}
	}
}

// A person's messages are handled one at a time: "对" sent while a
// correction is with the model waits for it, and saves the corrected record.
func TestOneMessageAtATimePerUser(t *testing.T) {
	tt := newTester(t, yuxiang, strings.Replace(yuxiang, `"猪肉末"`, `"猪肉末", "木耳"`, 1))
	tt.say("u1", "记 今天做了鱼香茄子，少放了糖，9 分")

	gate := make(chan struct{})
	tt.models.mu.Lock()
	tt.models.gate = gate
	tt.models.mu.Unlock()
	corrected := make(chan string)
	go func() { corrected <- tt.say("u1", "还放了木耳") }()
	waitFor(t, func() bool { return tt.models.calls() == 2 })

	saved := make(chan string)
	go func() { saved <- tt.say("u1", "对") }()
	// Someone else does not wait.
	wantReply(t, tt.say("u2", "你好"), help)
	select {
	case reply := <-saved:
		t.Fatalf("对 was answered during the correction: %s", reply)
	case <-time.After(100 * time.Millisecond):
	}
	close(gate)
	<-corrected
	wantReply(t, <-saved, "记好了。第一次做鱼香茄子。")
	tt.wantCooks("鱼香茄子|2026-11-14|9|少放了糖|u1|今天做了鱼香茄子，少放了糖，9 分\n还放了木耳|木耳,猪肉末,茄子")
}

// A message that waits for its turn gives up with its context.
func TestWaitingGivesUp(t *testing.T) {
	tt := newTester(t, yuxiang)
	gate := make(chan struct{})
	tt.models.gate = gate
	first := make(chan string)
	go func() {
		first <- tt.Handle(context.Background(), bot.Message{AuthorID: "u1", Content: "记 今天做了鱼香茄子"})
	}()
	waitFor(t, func() bool { return tt.models.calls() == 1 })

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	done := make(chan string)
	go func() { done <- tt.Handle(ctx, bot.Message{AuthorID: "u1", Content: "对"}) }()
	select {
	case reply := <-done:
		wantReply(t, reply, "")
	case <-time.After(3 * time.Second):
		t.Error("still waiting after its context ended")
	}
	close(gate)
	<-first
}

// The log says who and what happened, but not what anyone wrote.
func TestLogsHoldNoContent(t *testing.T) {
	// The problems with this reply quote it: date "明天" is not written as
	// YYYY-MM-DD.
	tomorrow := strings.Replace(yuxiang, "2026-11-14", "明天", 1)
	tt := newTester(t, yuxiang, tomorrow, tomorrow, strings.Replace(yuxiang, `"猪肉末"`, `"猪肉末", "木耳"`, 1))
	tt.say("u1", "记 今天做了鱼香茄子，少放了糖，9 分")
	tt.say("u1", "改成明天")
	tt.say("u1", "还放了木耳")
	tt.say("u1", "对")
	logs := tt.logs.String()
	if !strings.Contains(logs, "cook saved") || !strings.Contains(logs, "no valid record") {
		t.Errorf("the log misses what happened:\n%s", logs)
	}
	for _, word := range []string{"鱼香茄子", "木耳", "糖", "明天"} {
		if strings.Contains(logs, word) {
			t.Errorf("the log holds %q:\n%s", word, logs)
		}
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	for deadline := time.Now().Add(3 * time.Second); !cond(); time.Sleep(time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("timed out")
		}
	}
}
