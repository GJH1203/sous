package basics

import (
	"encoding/json"
	"errors"
	"testing"
	"time"
)

// Nil and empty slices behave the same for len, range and append, but not
// for == nil or JSON. The JSON difference matters once the gateway emits it.
func TestNilVersusEmptySlice(t *testing.T) {
	var nilSlice []string
	emptySlice := []string{}

	if len(nilSlice) != 0 || len(emptySlice) != 0 {
		t.Fatalf("len: nil=%d empty=%d, want 0 and 0", len(nilSlice), len(emptySlice))
	}
	if nilSlice != nil || emptySlice == nil {
		t.Errorf("== nil: nil=%v empty=%v, want true and false", nilSlice == nil, emptySlice == nil)
	}
	for _, tc := range []struct {
		name  string
		slice []string
		want  string
	}{
		{"nil", nilSlice, "null"},
		{"empty", emptySlice, "[]"},
	} {
		got, err := json.Marshal(tc.slice)
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != tc.want {
			t.Errorf("json.Marshal(%s slice) = %s, want %s", tc.name, got, tc.want)
		}
	}
}

// A nil *RatingError stored in an error interface is not a nil error: the
// interface holds a type and a nil pointer. Always return a literal nil.
func TestTypedNilIsNotNilError(t *testing.T) {
	var re *RatingError
	var err error = re
	if err == nil {
		t.Fatal("err == nil, want non-nil: the interface carries the *RatingError type")
	}
	if ValidateRating(5) != nil {
		t.Error("ValidateRating(5) != nil, want a literal nil")
	}
}

func TestReplyFallsBackForUnknownErrors(t *testing.T) {
	if got, want := Reply(errors.New("disk full")), "something went wrong"; got != want {
		t.Errorf("Reply(other error) = %q, want %q", got, want)
	}
}

func TestFakeClockAccumulates(t *testing.T) {
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	c := NewFakeClock(start)
	c.Advance(time.Hour)
	c.Advance(30 * time.Minute)
	if got, want := c.Now().Sub(start), 90*time.Minute; got != want {
		t.Errorf("after two Advances moved %v, want %v", got, want)
	}
}

func TestRealClockTracksWallTime(t *testing.T) {
	before := time.Now()
	got := RealClock{}.Now()
	if got.Before(before) || got.Sub(before) > time.Second {
		t.Errorf("RealClock.Now() = %v, want close to %v", got, before)
	}
}

// Modifying an element through a slice is visible to the caller because both
// share the backing array; Rate depends on this.
func TestRateEditsCallerSlice(t *testing.T) {
	dishes := []Dish{{Name: "omelette", Rating: 3}}
	if err := Rate(dishes, "omelette", 7); err != nil {
		t.Fatal(err)
	}
	if dishes[0].Rating != 7 {
		t.Errorf("caller sees rating %d, want 7", dishes[0].Rating)
	}
}
