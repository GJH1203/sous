package basics

import (
	"errors"
	"testing"
)

func TestRate(t *testing.T) {
	tests := []struct {
		name       string
		dish       string
		rating     int
		wantReply  string
		wantRating int // rating of "mapo tofu" afterwards
	}{
		{"ok", "mapo tofu", 8, "saved", 8},
		{"lowest", "mapo tofu", 1, "saved", 1},
		{"highest", "mapo tofu", 10, "saved", 10},
		{"too low", "mapo tofu", 0, "ratings go from 1 to 10, got 0", 9},
		{"too high", "mapo tofu", 11, "ratings go from 1 to 10, got 11", 9},
		{"unknown dish", "pizza", 8, "never cooked that one", 9},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Fresh slice per case so one case's write cannot leak into the next.
			dishes := []Dish{{Name: "mapo tofu", Rating: 9}, {Name: "omelette", Rating: 6}}
			err := Rate(dishes, tt.dish, tt.rating)
			if got := Reply(err); got != tt.wantReply {
				t.Errorf("Reply(err) = %q, want %q (err: %v)", got, tt.wantReply, err)
			}
			if got := dishes[0].Rating; got != tt.wantRating {
				t.Errorf("mapo tofu rating = %d, want %d", got, tt.wantRating)
			}
		})
	}
}

// The wrapped error must still expose its cause: that is what %w buys over %v.
func TestRateWrapsCause(t *testing.T) {
	err := Rate(nil, "pizza", 5)
	if !errors.Is(err, ErrUnknownDish) {
		t.Errorf("errors.Is(%v, ErrUnknownDish) = false, want true", err)
	}

	err = Rate(nil, "pizza", 42)
	var re *RatingError
	if !errors.As(err, &re) {
		t.Fatalf("errors.As(%v, *RatingError) = false, want true", err)
	}
	if re.Rating != 42 {
		t.Errorf("RatingError.Rating = %d, want 42", re.Rating)
	}
	if want := `rate "pizza": rating 42 is outside 1..10`; err.Error() != want {
		t.Errorf("err.Error() = %q, want %q", err.Error(), want)
	}
}
