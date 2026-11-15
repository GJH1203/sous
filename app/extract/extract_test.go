package extract

import (
	"context"
	"errors"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/GJH1203/sous/app/llm"
	"github.com/GJH1203/sous/internal/fakeupstream"
)

// script stands in for the model: it gives its replies in order, and keeps
// every request.
type script struct {
	replies []string
	err     error
	got     []llm.ChatRequest
	feature string
}

func (s *script) Chat(ctx context.Context, feature string, req llm.ChatRequest) (string, error) {
	s.feature = feature
	// A copy: Extract appends to the slice it sent.
	req.Messages = append([]llm.Message(nil), req.Messages...)
	s.got = append(s.got, req)
	if s.err != nil {
		return "", s.err
	}
	if len(s.got) > len(s.replies) {
		return "", errors.New("the script has run out of replies")
	}
	return s.replies[len(s.got)-1], nil
}

// now is a Saturday evening in a zone west of UTC, where it is already
// Sunday: the date must come from the local day.
var now = time.Date(2026, 11, 14, 20, 30, 0, 0, time.FixedZone("UTC-8", -8*3600))

func rating(v float64) *float64 { return &v }

const good = `{"dish": "鱼香茄子", "ingredients": ["茄子", "猪肉末"], "rating": 9, "note": "少放了糖", "date": "2026-11-14"}`

var goodRecord = Record{Dish: "鱼香茄子", Ingredients: []string{"茄子", "猪肉末"}, Rating: rating(9), Note: "少放了糖", Date: "2026-11-14"}

func TestExtract(t *testing.T) {
	s := &script{replies: []string{good}}
	r, err := Extract(context.Background(), s, "今天做了鱼香茄子，少放了糖，9 分", now)
	if err != nil || !reflect.DeepEqual(r, goodRecord) {
		t.Fatalf("got %+v, %v", r, err)
	}
	if len(s.got) != 1 || s.feature != "record" {
		t.Fatalf("%d requests, feature %q", len(s.got), s.feature)
	}
	req := s.got[0]
	if req.Model != llm.Extract || req.Temperature == nil || *req.Temperature != 0 {
		t.Errorf("model %q, temperature %v", req.Model, req.Temperature)
	}
	if len(req.Messages) != 2 || req.Messages[0].Role != "system" || req.Messages[0].Content != prompt {
		t.Fatalf("messages %+v", req.Messages)
	}
	want := "Today: 2026-11-14, Saturday\nMessage: 今天做了鱼香茄子，少放了糖，9 分"
	if req.Messages[1] != (llm.Message{Role: "user", Content: want}) {
		t.Errorf("user message %+v", req.Messages[1])
	}
}

// A wrong first reply goes back with what was wrong, and a right second
// one is taken.
func TestExtractRetries(t *testing.T) {
	bad := `{"dish": "鱼香茄子", "ingredients": [], "rating": 90, "note": "", "date": "2026-11-14"}`
	s := &script{replies: []string{bad, good}}
	r, err := Extract(context.Background(), s, "今天做了鱼香茄子，少放了糖，9 分", now)
	if err != nil || !reflect.DeepEqual(r, goodRecord) {
		t.Fatalf("got %+v, %v", r, err)
	}
	if len(s.got) != 2 {
		t.Fatalf("%d requests, want 2", len(s.got))
	}
	second := s.got[1].Messages
	if len(second) != 4 || second[2] != (llm.Message{Role: "assistant", Content: bad}) || second[3].Role != "user" {
		t.Fatalf("second request: %+v", second)
	}
	// Both problems, so that one more try can fix them both.
	for _, p := range []string{"ingredients is empty", "rating 90 is not between 0 and 10"} {
		if !strings.Contains(second[3].Content, p) {
			t.Errorf("the feedback does not say %q:\n%s", p, second[3].Content)
		}
	}
}

func TestExtractGivesUp(t *testing.T) {
	s := &script{replies: []string{"鱼香茄子，9 分", `{"dish": ""}`, good}}
	_, err := Extract(context.Background(), s, "今天做了鱼香茄子", now)
	if !errors.Is(err, ErrNotUnderstood) || !strings.Contains(err.Error(), "dish is missing") {
		t.Errorf("got %v", err)
	}
	if len(s.got) != attempts {
		t.Errorf("%d requests, want %d", len(s.got), attempts)
	}
}

// A failed call is not the model's answer, and is not retried here.
func TestExtractPassesOnGatewayErrors(t *testing.T) {
	down := &llm.Error{Status: 502, Type: "upstream_error", Message: "connection refused"}
	s := &script{err: down}
	_, err := Extract(context.Background(), s, "今天做了鱼香茄子", now)
	if !errors.Is(err, down) || errors.Is(err, ErrNotUnderstood) || len(s.got) != 1 {
		t.Errorf("got %v after %d requests", err, len(s.got))
	}
}

func TestParse(t *testing.T) {
	tests := []struct {
		name  string
		reply string
		want  Record // when problem is ""
		// problem is part of the error, or "" for none.
		problem string
	}{
		{"good", good, goodRecord, ""},
		{"in a code fence", "```json\n" + good + "\n```", goodRecord, ""},
		{"no rating", `{"dish":"番茄炒蛋","ingredients":["番茄","鸡蛋"],"rating":null,"note":"","date":"2026-11-13"}`,
			Record{Dish: "番茄炒蛋", Ingredients: []string{"番茄", "鸡蛋"}, Date: "2026-11-13"}, ""},
		{"half a point", `{"dish":"麻婆豆腐","ingredients":["豆腐"],"rating":8.5,"note":"","date":"2026-11-14"}`,
			Record{Dish: "麻婆豆腐", Ingredients: []string{"豆腐"}, Rating: rating(8.5), Date: "2026-11-14"}, ""},
		{"tidied", `{"dish":" 麻婆豆腐 ","ingredients":["豆腐"," 豆腐","","牛肉末"],"rating":0,"note":" ","date":"2026-11-14"}`,
			Record{Dish: "麻婆豆腐", Ingredients: []string{"豆腐", "牛肉末"}, Rating: rating(0), Date: "2026-11-14"}, ""},
		{"a year ago today", `{"dish":"饺子","ingredients":["面粉"],"date":"2025-11-14"}`,
			Record{Dish: "饺子", Ingredients: []string{"面粉"}, Date: "2025-11-14"}, ""},

		{"not JSON", "鱼香茄子，9 分", Record{}, "not the JSON object asked for"},
		{"cut off", good[:40], Record{}, "not the JSON object asked for"},
		{"a list", "[" + good + "]", Record{}, "not the JSON object asked for"},
		{"text after it", good + "\nHope this helps!", Record{}, "more after the JSON object"},
		{"two objects", good + good, Record{}, "more after the JSON object"},
		{"unknown field", `{"dish":"饺子","ingredients":["面粉"],"date":"2026-11-14","time":"evening"}`, Record{}, `unknown field "time"`},
		{"rating as text", `{"dish":"饺子","ingredients":["面粉"],"rating":"9","date":"2026-11-14"}`, Record{}, "rating must not be a JSON string"},
		{"ingredients as text", `{"dish":"饺子","ingredients":"面粉","date":"2026-11-14"}`, Record{}, "ingredients must not be a JSON string"},
		{"null", "null", Record{}, "dish is missing"},
		{"no dish", `{"dish":" ","ingredients":["面粉"],"date":"2026-11-14"}`, Record{}, "dish is missing"},
		{"whole message as dish", `{"dish":"今天晚上下班回家以后我给我们两个人做了一盘特别好吃的鱼香茄子","ingredients":["茄子"],"date":"2026-11-14"}`, Record{}, "too long"},
		{"no ingredients", `{"dish":"饺子","ingredients":[" "],"date":"2026-11-14"}`, Record{}, "ingredients is empty"},
		{"rating above 10", `{"dish":"饺子","ingredients":["面粉"],"rating":10.5,"date":"2026-11-14"}`, Record{}, "rating 10.5 is not between 0 and 10"},
		{"rating below 0", `{"dish":"饺子","ingredients":["面粉"],"rating":-1,"date":"2026-11-14"}`, Record{}, "rating -1 is not between 0 and 10"},
		{"no date", `{"dish":"饺子","ingredients":["面粉"]}`, Record{}, "date is missing"},
		{"date in words", `{"dish":"饺子","ingredients":["面粉"],"date":"11月14日"}`, Record{}, `date "11月14日" is not written as YYYY-MM-DD`},
		{"tomorrow", `{"dish":"饺子","ingredients":["面粉"],"date":"2026-11-15"}`, Record{}, "date 2026-11-15 is after today, 2026-11-14"},
		{"over a year ago", `{"dish":"饺子","ingredients":["面粉"],"date":"2025-11-13"}`, Record{}, "more than a year before today"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r, err := parse(tt.reply, now)
			if tt.problem == "" {
				if err != nil || !reflect.DeepEqual(r, tt.want) {
					t.Errorf("got %+v, %v; want %+v", r, err, tt.want)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.problem) {
				t.Errorf("got %v, want a problem with %q", err, tt.problem)
			}
		})
	}
}

// Every problem is reported, not just the first.
func TestParseReportsEveryProblem(t *testing.T) {
	_, err := parse(`{"dish":"","ingredients":[],"rating":11,"date":""}`, now)
	want := "dish is missing\ningredients is empty; give the main ingredients of the dish\n" +
		"rating 11 is not between 0 and 10\ndate is missing"
	if err == nil || err.Error() != want {
		t.Errorf("got\n%v\nwant\n%s", err, want)
	}
}

// The prompt asks for every field that Record has.
func TestPromptNamesEveryField(t *testing.T) {
	typ := reflect.TypeFor[Record]()
	for i := range typ.NumField() {
		if tag := typ.Field(i).Tag.Get("json"); !strings.Contains(prompt, `"`+tag+`"`) {
			t.Errorf("the prompt does not mention %q", tag)
		}
	}
}

// Through the real client and the fake: a model that keeps sending broken
// JSON costs two requests, then an error, and no crash.
func TestBrokenJSONFromTheUpstream(t *testing.T) {
	cfg := fakeupstream.DefaultConfig()
	cfg.ChunkInterval = 0
	cfg.Reply = good[:50] // cut off halfway
	fake, err := fakeupstream.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(fake)
	defer srv.Close()
	defer fake.Close()
	_, err = Extract(context.Background(), llm.New(srv.URL, "key"), "今天做了鱼香茄子", now)
	if !errors.Is(err, ErrNotUnderstood) {
		t.Errorf("got %v", err)
	}
	if n := fake.Stats().Requests; n != attempts {
		t.Errorf("%d requests, want %d", n, attempts)
	}
}
