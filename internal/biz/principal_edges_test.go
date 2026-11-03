package biz

import (
	"testing"

	"github.com/google/uuid"
)

func TestPrincipalValidationAndUsernameNormalization(t *testing.T) {
	if _, err := NewPrincipal(uuid.Nil, RoleUser); err == nil {
		t.Fatal("nil user id was accepted")
	}
	if _, err := NewPrincipal(uuid.New(), Role("invalid")); err == nil {
		t.Fatal("invalid role was accepted")
	}
	if got := NormalizeUsername("  Alice\t"); got != "alice" {
		t.Fatalf("NormalizeUsername() = %q", got)
	}
}
