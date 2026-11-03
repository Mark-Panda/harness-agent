package biz

import (
	"context"
	"testing"

	"github.com/google/uuid"
)

func TestPrincipalContextAndUserAccess(t *testing.T) {
	userID := uuid.New()
	principal, err := NewPrincipal(userID, RoleUser)
	if err != nil {
		t.Fatal(err)
	}
	ctx := WithPrincipal(context.Background(), principal)
	got, ok := PrincipalFromContext(ctx)
	if !ok || got != principal {
		t.Fatalf("PrincipalFromContext() = %#v, %t", got, ok)
	}
	if !got.CanAccessUser(userID) || got.CanAccessUser(uuid.New()) {
		t.Fatal("principal user access is not isolated")
	}
	admin, _ := NewPrincipal(uuid.New(), RoleAdmin)
	if admin.CanAccessUser(userID) {
		t.Fatal("admin role must not implicitly grant user content access")
	}
}
