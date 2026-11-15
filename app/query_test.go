package app

import (
	"context"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/GJH1203/sous/app/bot"
	"github.com/GJH1203/sous/app/llm"
	"github.com/GJH1203/sous/app/store"
	"github.com/GJH1203/sous/internal/fakeupstream"
)

func rated(r float64) *float64 { return &r }

// The rule is a pure function, tested without a database.
func TestRank(t *testing.T) {
	dishes := []store.DishStats{
		{Dish: "家常豆腐", Last: "2026-11-12"},                     // never rated
		{Dish: "麻婆豆腐", Rating: rated(8.5), Last: "2026-11-11"}, // 8.5, cooked recently
		{Dish: "豆腐煲", Rating: rated(8.47), Last: "2026-09-01"}, // shows as 8.5, cooked long ago
		{Dish: "锅塌豆腐", Rating: rated(9), Last: "2026-11-14"},
		{Dish: "豆腐汤", Rating: rated(6), Last: "2026-01-01"},
		{Dish: "凉拌豆腐", Last: "2026-10-01"}, // never rated, but longer ago
	}
	before := slices.Clone(dishes)
	var got []string
	for _, d := range rank(dishes) {
		got = append(got, d.Dish)
	}
	want := []string{"锅塌豆腐", "豆腐煲", "麻婆豆腐", "豆腐汤", "凉拌豆腐", "家常豆腐"}
	if !slices.Equal(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
	if !slices.EqualFunc(dishes, before, func(x, y store.DishStats) bool { return x.Dish == y.Dish }) {
		t.Error("rank reordered its argument")
	}
	// Same rating, same day: by name, so the order does not depend on the
	// order the database gave.
	a := []store.DishStats{{Dish: "b", Last: "2026-11-01"}, {Dish: "a", Last: "2026-11-01"}}
	if r := rank(a); r[0].Dish != "a" {
		t.Errorf("got %v", r)
	}
}

func TestSince(t *testing.T) {
	now := time.Date(2026, 11, 14, 23, 30, 0, 0, zone) // in UTC, already the 15th
	for date, want := range map[string]string{
		"2026-11-14": "今天",
		"2026-11-13": "昨天",
		"2026-11-11": "3 天前（11 月 11 日）",
		"2025-12-31": "318 天前（12 月 31 日）",
	} {
		if got := since(date, now); got != want {
			t.Errorf("since(%s) = %q, want %q", date, got, want)
		}
	}
	// East of UTC, where local midnight is the day before in UTC.
	east := time.Date(2026, 11, 14, 0, 30, 0, 0, time.FixedZone("UTC+8", 8*3600))
	if got := since("2026-11-13", east); got != "昨天" {
		t.Errorf("east of UTC: %q", got)
	}
}

// record saves a dish the way a person would: 记, then 对.
func (tt *tester) record(author, reply string) {
	tt.t.Helper()
	tt.models.mu.Lock()
	tt.models.replies = append(tt.models.replies, reply)
	tt.models.mu.Unlock()
	tt.say(author, "记 "+reply)
	if got := tt.say(author, "对"); !strings.HasPrefix(got, "记好了") {
		tt.t.Fatalf("saving %s: %s", reply, got)
	}
}

func TestWant(t *testing.T) {
	tt := newTester(t)
	tt.record("u1", `{"dish": "麻婆豆腐", "ingredients": ["豆腐", "牛肉末"], "rating": 8, "note": "花椒放少了", "date": "2026-11-11"}`)
	tt.record("u2", `{"dish": "麻婆豆腐", "ingredients": ["豆腐"], "rating": 9, "note": "", "date": "2026-11-13"}`)
	tt.record("u1", `{"dish": "家常豆腐", "ingredients": ["豆腐", "青椒"], "rating": null, "note": "", "date": "2026-11-14"}`)
	tt.record("u1", `{"dish": "鱼香茄子", "ingredients": ["茄子"], "rating": 9, "note": "", "date": "2026-11-14"}`)

	want := "做过的带「豆腐」的菜：\n" +
		"1. 麻婆豆腐：做过 2 次，平均 8.5 分，上次是昨天。最近的备注：花椒放少了\n" +
		"2. 家常豆腐：做过 1 次，没打过分，上次是今天"
	for _, text := range []string{"想吃 豆腐", "想吃豆腐", "想吃：豆腐"} {
		wantReply(t, tt.say("u1", text), want)
	}
	wantReply(t, tt.say("u1", "想吃 榴莲"), "还没做过带「榴莲」的菜。")
	wantReply(t, tt.say("u1", "想吃"), "想吃什么？比如「想吃 豆腐」。")
}

func TestWantListsTen(t *testing.T) {
	tt := newTester(t)
	for i := range maxListed + 2 {
		tt.record("u1", `{"dish": "豆腐`+string(rune('a'+i))+`", "ingredients": ["豆腐"], "rating": null, "note": "", "date": "2026-11-14"}`)
	}
	reply := tt.say("u1", "想吃 豆腐")
	if lines := strings.Split(reply, "\n"); len(lines) != maxListed+2 || lines[len(lines)-1] != "还有 2 道没列出来。" {
		t.Errorf("reply\n%s", reply)
	}
}

func TestAliasCommand(t *testing.T) {
	tt := newTester(t)
	tt.record("u1", `{"dish": "番茄炒蛋", "ingredients": ["西红柿", "鸡蛋"], "rating": 8, "note": "", "date": "2026-11-14"}`)

	wantReply(t, tt.say("u1", "别名"), "还没有别名。"+aliasUsage)
	wantReply(t, tt.say("u1", "别名 西红柿 = 番茄"), "好，以后「西红柿」就算「番茄」。之前记成「西红柿」的 1 次也改过来了。")
	wantReply(t, tt.say("u1", "别名 洋柿子＝西红柿"), "好，以后「洋柿子」就算「番茄」。")
	wantReply(t, tt.say("u1", "别名 西红柿=番茄"), "「西红柿」和「番茄」已经算同一种了。")
	wantReply(t, tt.say("u1", "别名"), "别名：\n洋柿子 = 番茄\n西红柿 = 番茄")
	for _, bad := range []string{"别名 西红柿", "别名 = 番茄", "别名 西红柿 =", "别名 a = b = c"} {
		wantReply(t, tt.say("u1", bad), aliasUsage)
	}
	// Queries go through the aliases too.
	if reply := tt.say("u1", "想吃 洋柿子"); !strings.Contains(reply, "番茄炒蛋") {
		t.Errorf("reply\n%s", reply)
	}
}

// Acceptance: through the real client and the fake upstream, a query costs
// no model request.
func TestQueriesCallNoModel(t *testing.T) {
	cfg := fakeupstream.DefaultConfig()
	cfg.ChunkInterval = 0
	cfg.Reply = yuxiang
	fake, err := fakeupstream.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(fake)
	defer srv.Close()
	defer fake.Close()

	tt := newTesterWith(t, llm.New(srv.URL, "key"))
	tt.say("u1", "记 今天做了鱼香茄子，少放了糖，9 分")
	wantReply(t, tt.say("u1", "对"), "记好了。第一次做鱼香茄子。")
	before := fake.Stats().Requests
	if reply := tt.say("u1", "想吃 茄子"); !strings.Contains(reply, "1. 鱼香茄子：做过 1 次，平均 9 分") {
		t.Errorf("reply\n%s", reply)
	}
	tt.say("u1", "别名 紫茄子 = 茄子")
	tt.say("u1", "别名")
	if after := fake.Stats().Requests; after != before || before != 1 {
		t.Errorf("model requests: %d before the queries, %d after; want 1, 1", before, after)
	}
}

// A query does not wait behind the person's slow 记.
func TestQueriesDoNotWait(t *testing.T) {
	tt := newTester(t, yuxiang)
	gate := make(chan struct{})
	tt.models.gate = gate
	first := make(chan string)
	go func() { first <- tt.say("u1", "记 今天做了鱼香茄子") }()
	waitFor(t, func() bool { return tt.models.calls() == 1 })

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	for _, text := range []string{"想吃 茄子", "别名"} {
		if reply := tt.Handle(ctx, bot.Message{AuthorID: "u1", Content: text}); reply == "" {
			t.Errorf("%s waited for the turn", text)
		}
	}
	close(gate)
	<-first
}
