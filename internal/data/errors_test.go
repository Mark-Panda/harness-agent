package data

import (
	"context"
	"errors"
	"testing"

	"gorm.io/gorm"
)

func TestMapError(t *testing.T) {
	for input, expected := range map[error]error{
		gorm.ErrRecordNotFound:   ErrNotFound,
		gorm.ErrDuplicatedKey:    ErrConflict,
		context.DeadlineExceeded: ErrTimeout,
	} {
		if got := MapError(input); !errors.Is(got, expected) {
			t.Fatalf("MapError(%v) = %v, want %v", input, got, expected)
		}
	}
	if MapError(nil) != nil {
		t.Fatal("nil error must remain nil")
	}
}
