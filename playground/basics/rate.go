package basics

import (
	"errors"
	"fmt"
)

const (
	MinRating = 1
	MaxRating = 10
)

// ErrUnknownDish is a sentinel: one shared value that callers match with
// errors.Is, even after other layers have wrapped it.
var ErrUnknownDish = errors.New("unknown dish")

// RatingError keeps the rejected value so the outermost layer can show it
// back to the user; errors.As digs it out of a wrapped chain.
type RatingError struct {
	Rating int
}

// Error has a pointer receiver, so *RatingError is the type that satisfies
// error. That is also the type errors.As has to be asked for.
func (e *RatingError) Error() string {
	return fmt.Sprintf("rating %d is outside %d..%d", e.Rating, MinRating, MaxRating)
}

// ValidateRating returns nil or a *RatingError.
func ValidateRating(r int) error {
	if r < MinRating || r > MaxRating {
		return &RatingError{Rating: r}
	}
	return nil
}

// Rate sets the rating of the named dish in place. %w adds context
// ("rate \"pizza\"") while keeping the cause reachable for errors.Is/As.
func Rate(dishes []Dish, name string, rating int) error {
	if err := ValidateRating(rating); err != nil {
		return fmt.Errorf("rate %q: %w", name, err)
	}
	for i := range dishes {
		if dishes[i].Name == name {
			// Write through the index: in "for _, d := range" d is a copy
			// and the new rating would be silently lost.
			dishes[i].Rating = rating
			return nil
		}
	}
	return fmt.Errorf("rate %q: %w", name, ErrUnknownDish)
}

// Reply turns an error into what the bot would say. Only the outermost layer
// inspects error identity; the layers below just add context and pass it up.
func Reply(err error) string {
	var re *RatingError
	switch {
	case err == nil:
		return "saved"
	case errors.Is(err, ErrUnknownDish):
		return "never cooked that one"
	case errors.As(err, &re):
		return fmt.Sprintf("ratings go from %d to %d, got %d", MinRating, MaxRating, re.Rating)
	default:
		return "something went wrong"
	}
}
