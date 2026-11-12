package basics

import (
	"testing"
	"time"
)

func TestDaysSince(t *testing.T) {
	start := time.Date(2026, 11, 1, 18, 0, 0, 0, time.UTC)
	tests := []struct {
		name    string
		advance time.Duration
		want    int
	}{
		{"same moment", 0, 0},
		{"just under a day", 23*time.Hour + 59*time.Minute, 0},
		{"exactly a day", 24 * time.Hour, 1},
		{"ten and a half days", 10*24*time.Hour + 12*time.Hour, 10},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			clock := NewFakeClock(start)
			clock.Advance(tt.advance)
			if got := DaysSince(clock, start); got != tt.want {
				t.Errorf("DaysSince after %v = %d, want %d", tt.advance, got, tt.want)
			}
		})
	}
}
