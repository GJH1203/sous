// Package extract reads a cooking record out of a message, with a model.
// The model's reply is untrusted input: it is parsed strictly and checked,
// and a reply that fails goes back to the model once, with what was wrong.
package extract

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/GJH1203/sous/app/llm"
)

//go:embed prompt.txt
var prompt string

// Record is what one message says about one time a dish was cooked.
type Record struct {
	Dish        string   `json:"dish"`
	Ingredients []string `json:"ingredients"`
	Rating      *float64 `json:"rating"` // nil when the message gives no score
	Note        string   `json:"note"`
	Date        string   `json:"date"` // YYYY-MM-DD, in the app's time zone
}

// Chatter sends a chat request. *llm.Client is one; tests use a script.
type Chatter interface {
	Chat(ctx context.Context, feature string, req llm.ChatRequest) (string, error)
}

// ErrNotUnderstood means that the model gave no valid record, even after it
// was told what was wrong with its first reply.
var ErrNotUnderstood = errors.New("no valid record in the model's replies")

// attempts is how many replies the model gets: its first, and one more
// after it is told what was wrong. A third rarely fixes what a second did
// not, and each one is a wait for the person.
const attempts = 2

// temperature 0 makes the same message give the same record, which matters
// for the evaluation as much as for the person.
var temperature = 0.0

// Extract reads a record out of text, a message sent at now. now must be in
// the app's time zone: "today" and "yesterday" are days there.
func Extract(ctx context.Context, chat Chatter, text string, now time.Time) (Record, error) {
	msgs := []llm.Message{
		{Role: "system", Content: prompt},
		{Role: "user", Content: fmt.Sprintf("Today: %s, %s\nMessage: %s", now.Format(time.DateOnly), now.Weekday(), text)},
	}
	var problems error
	for range attempts {
		reply, err := chat.Chat(ctx, "record", llm.ChatRequest{Model: llm.Extract, Messages: msgs, Temperature: &temperature})
		if err != nil {
			// The gateway's trouble, not the model's answer: asking again
			// here would not help.
			return Record{}, err
		}
		var r Record
		r, problems = parse(reply, now)
		if problems == nil {
			return r, nil
		}
		// The whole conversation goes back, so the model sees its own reply
		// next to what is wrong with it.
		msgs = append(msgs,
			llm.Message{Role: "assistant", Content: reply},
			llm.Message{Role: "user", Content: "That reply has these problems:\n" + problems.Error() +
				"\nReply again with the corrected JSON object only."},
		)
	}
	return Record{}, fmt.Errorf("%w: %w", ErrNotUnderstood, problems)
}

// parse reads a record out of a reply, tidies it, and checks it.
func parse(reply string, now time.Time) (Record, error) {
	text := strings.TrimSpace(reply)
	// Models wrap JSON in a Markdown code fence even when told not to. The
	// fence changes nothing about the content, so it is let through.
	if strings.HasPrefix(text, "```") && strings.HasSuffix(text, "```") {
		text = strings.TrimSuffix(strings.TrimPrefix(text, "```"), "```")
		text = strings.TrimSpace(strings.TrimPrefix(text, "json"))
	}
	dec := json.NewDecoder(strings.NewReader(text))
	// A field the prompt did not ask for means the model is not following
	// it, and the fields it did ask for are suspect too.
	dec.DisallowUnknownFields()
	var r Record
	if err := dec.Decode(&r); err != nil {
		var typeErr *json.UnmarshalTypeError
		// Field is empty when the reply as a whole has the wrong type.
		if errors.As(err, &typeErr) && typeErr.Field != "" {
			return Record{}, fmt.Errorf("%s must not be a JSON %s", typeErr.Field, typeErr.Value)
		}
		return Record{}, fmt.Errorf("the reply is not the JSON object asked for: %v", err)
	}
	// Decode stops after one value; anything after it is more than was
	// asked for.
	if _, err := dec.Token(); err != io.EOF {
		return Record{}, errors.New("there is more after the JSON object")
	}
	r.tidy()
	return r, r.check(now)
}

// tidy trims the text fields, and drops empty and repeated ingredients.
func (r *Record) tidy() {
	r.Dish = strings.TrimSpace(r.Dish)
	r.Note = strings.TrimSpace(r.Note)
	r.Date = strings.TrimSpace(r.Date)
	seen := make(map[string]bool)
	var kept []string
	for _, name := range r.Ingredients {
		name = strings.TrimSpace(name)
		if name != "" && !seen[name] {
			seen[name] = true
			kept = append(kept, name)
		}
	}
	r.Ingredients = kept
}

// maxDish is longer than any dish's name. A longer one is most likely the
// whole message copied into the field.
const maxDish = 20

// check reports every problem with r at once, in words that the model can
// act on: the first try is the only chance to fix them all.
func (r Record) check(now time.Time) error {
	var errs []error
	problem := func(format string, args ...any) {
		errs = append(errs, fmt.Errorf(format, args...))
	}
	switch {
	case r.Dish == "":
		problem("dish is missing")
	case utf8.RuneCountInString(r.Dish) > maxDish:
		problem("dish is too long for the name of a dish")
	}
	if len(r.Ingredients) == 0 {
		problem("ingredients is empty; give the main ingredients of the dish")
	}
	if r.Rating != nil && (*r.Rating < 0 || *r.Rating > 10) {
		problem("rating %v is not between 0 and 10", *r.Rating)
	}
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	day, err := time.ParseInLocation(time.DateOnly, r.Date, now.Location())
	switch {
	case r.Date == "":
		problem("date is missing")
	case err != nil:
		problem("date %q is not written as YYYY-MM-DD", r.Date)
	case day.After(today):
		problem("date %s is after today, %s", r.Date, today.Format(time.DateOnly))
	case day.Before(today.AddDate(-1, 0, 0)):
		problem("date %s is more than a year before today, %s", r.Date, today.Format(time.DateOnly))
	}
	return errors.Join(errs...)
}
