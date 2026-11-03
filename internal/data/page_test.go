package data

import (
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestCursorRoundTrip(t *testing.T) {
	expected := Cursor{CreatedAt: time.Date(2026, time.November, 2, 12, 0, 0, 0, time.UTC), ID: uuid.New()}
	encoded, err := EncodeCursor(expected)
	if err != nil {
		t.Fatal(err)
	}
	actual, err := DecodeCursor(encoded)
	if err != nil {
		t.Fatal(err)
	}
	if actual.ID != expected.ID || !actual.CreatedAt.Equal(expected.CreatedAt) {
		t.Fatalf("cursor = %#v, want %#v", actual, expected)
	}
	if _, err := DecodeCursor("not-a-cursor"); err == nil {
		t.Fatal("invalid cursor was accepted")
	}
}

func TestNormalizeLimit(t *testing.T) {
	for input, expected := range map[int]int{0: DefaultPageLimit, -1: DefaultPageLimit, 10: 10, MaxPageLimit + 1: MaxPageLimit} {
		if got := NormalizeLimit(input); got != expected {
			t.Fatalf("NormalizeLimit(%d) = %d, want %d", input, got, expected)
		}
	}
}
