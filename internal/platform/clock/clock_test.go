package clock

import (
	"testing"
	"time"
)

func TestFixedClockAdvance(t *testing.T) {
	start := time.Date(2026, time.November, 2, 12, 0, 0, 0, time.UTC)
	clock := NewFixed(start)
	clock.Advance(30 * time.Second)
	if got := clock.Now(); !got.Equal(start.Add(30 * time.Second)) {
		t.Fatalf("Now() = %s, want %s", got, start.Add(30*time.Second))
	}
}
