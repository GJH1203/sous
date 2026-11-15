package extract

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"slices"
	"testing"
	"time"

	"github.com/GJH1203/sous/app/llm"
)

// The tests here run against a real model, and are skipped unless
// SOUS_EXTRACT_URL is set: to the gateway's address, with SOUS_APP_KEY set
// too, or to a model server's own. With the gateway on its real config:
//
//	SOUS_EXTRACT_URL=http://127.0.0.1:8080 go test ./app/extract -run Real -v

// realModel returns a client for the real model, and a Saturday evening.
func realModel(t *testing.T) (*llm.Client, time.Time) {
	t.Helper()
	url := os.Getenv("SOUS_EXTRACT_URL")
	if url == "" {
		t.Skip("SOUS_EXTRACT_URL is not set")
	}
	loc, err := time.LoadLocation("America/Chicago")
	if err != nil {
		t.Fatal(err)
	}
	return llm.New(url, os.Getenv("SOUS_APP_KEY")), time.Date(2026, 11, 14, 20, 30, 0, 0, loc)
}

func TestRealModel(t *testing.T) {
	client, now := realModel(t)
	none := (*float64)(nil)
	tests := []struct {
		text   string
		dish   string
		has    string // an ingredient that must be there
		rating *float64
		note   bool // whether the message says anything for the note
		date   string
	}{
		{"今天做了鱼香茄子，少放了糖，9 分", "鱼香茄子", "茄子", rating(9), true, "2026-11-14"},
		{"昨天做的红烧肉，有点肥，下次买瘦一点的五花肉，8 分", "红烧肉", "五花肉", rating(8), true, "2026-11-13"},
		{"番茄炒蛋 7 分", "番茄炒蛋", "鸡蛋", rating(7), false, "2026-11-14"},
		{"周三做了麻婆豆腐，用的牛肉末，花椒放少了", "麻婆豆腐", "牛肉末", none, true, "2026-11-11"},
		{"前天炒了个土豆丝，有点糊了", "土豆丝", "土豆", none, true, "2026-11-12"},
		{"今天包了饺子，韭菜猪肉馅的，9.5 分", "饺子", "韭菜", rating(9.5), false, "2026-11-14"},
		{"上周日做了可乐鸡翅，她很喜欢", "可乐鸡翅", "鸡翅", none, true, "2026-11-08"},
		// No ingredients and no score: the main ingredient comes from the
		// name, and nothing is made up.
		{"凉拌黄瓜", "凉拌黄瓜", "黄瓜", none, false, "2026-11-14"},
	}
	for _, tt := range tests {
		start := time.Now()
		r, err := Extract(context.Background(), client, tt.text, now)
		b, _ := json.Marshal(r)
		t.Logf("%s -> %s %v (%.2fs)", tt.text, b, err, time.Since(start).Seconds())
		if err != nil {
			t.Errorf("%s: %v", tt.text, err)
			continue
		}
		if r.Dish != tt.dish || !slices.Contains(r.Ingredients, tt.has) || r.Date != tt.date {
			t.Errorf("%s: dish %q, ingredients %v, date %s", tt.text, r.Dish, r.Ingredients, r.Date)
		}
		if (r.Rating == nil) != (tt.rating == nil) || (r.Rating != nil && *r.Rating != *tt.rating) {
			t.Errorf("%s: rating %v, want %v", tt.text, r.Rating, tt.rating)
		}
		if (r.Note != "") != tt.note {
			t.Errorf("%s: note %q", tt.text, r.Note)
		}
	}

	// Not about cooking at all: no record, rather than a made-up one.
	if r, err := Extract(context.Background(), client, "今天天气不错", now); !errors.Is(err, ErrNotUnderstood) {
		t.Errorf("今天天气不错 -> %+v, %v", r, err)
	}
}

func TestRealRevise(t *testing.T) {
	client, now := realModel(t)
	tests := []struct {
		text, fix string
		ok        func(Record) bool // the correction is in, and nothing else changed
	}{
		{"番茄炒蛋 7 分", "还放了木耳", func(r Record) bool {
			return slices.Contains(r.Ingredients, "木耳") && slices.Contains(r.Ingredients, "鸡蛋") && r.Rating != nil && *r.Rating == 7
		}},
		{"番茄炒蛋 7 分", "评分改成 8 分", func(r Record) bool {
			return r.Rating != nil && *r.Rating == 8 && r.Dish == "番茄炒蛋" && r.Date == "2026-11-14"
		}},
		{"番茄炒蛋 7 分", "是昨天做的", func(r Record) bool {
			return r.Date == "2026-11-13" && r.Rating != nil && *r.Rating == 7
		}},
		{"今天做了鱼香茄子，9 分", "备注：糖放少了", func(r Record) bool {
			return r.Note != "" && r.Rating != nil && *r.Rating == 9 && r.Dish == "鱼香茄子"
		}},
		{"今天做了鱼香茄子，9 分", "其实没打分", func(r Record) bool {
			return r.Rating == nil && r.Dish == "鱼香茄子"
		}},
		{"今天做了鱼香茄子，9 分", "菜名是鱼香肉丝", func(r Record) bool {
			return r.Dish == "鱼香肉丝" && r.Rating != nil && *r.Rating == 9
		}},
	}
	for _, tt := range tests {
		first, err := Extract(context.Background(), client, tt.text, now)
		if err != nil {
			t.Errorf("%s: %v", tt.text, err)
			continue
		}
		start := time.Now()
		r, err := Revise(context.Background(), client, tt.text, first, tt.fix, now)
		b, _ := json.Marshal(r)
		t.Logf("%s + %s -> %s %v (%.2fs)", tt.text, tt.fix, b, err, time.Since(start).Seconds())
		if err != nil || !tt.ok(r) {
			t.Errorf("%s + %s: %+v, %v", tt.text, tt.fix, r, err)
		}
	}
}
