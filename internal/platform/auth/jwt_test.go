package auth

import (
	"testing"
	"time"

	"github.com/Mark-Panda/harness-agent/internal/platform/clock"
	"github.com/google/uuid"
)

func testKey(id string) SigningKey {
	return SigningKey{ID: id, Secret: []byte(id + "-0123456789012345678901234567890123456789")}
}

func TestJWTKeyRotation(t *testing.T) {
	now := time.Date(2026, time.November, 2, 12, 0, 0, 0, time.UTC)
	fixed := clock.NewFixed(now)
	oldManager, err := NewJWTManager(testKey("old"), nil, "harness-test", fixed)
	if err != nil {
		t.Fatal(err)
	}
	token, err := oldManager.IssueAccess(uuid.NewString(), "user", 1)
	if err != nil {
		t.Fatal(err)
	}
	newManager, err := NewJWTManager(testKey("new"), []SigningKey{testKey("old")}, "harness-test", fixed)
	if err != nil {
		t.Fatal(err)
	}
	claims, err := newManager.ParseAccess(token)
	if err != nil || claims.Role != "user" {
		t.Fatalf("rotated token parse = %#v, %v", claims, err)
	}
	unknownManager, _ := NewJWTManager(testKey("unknown"), nil, "harness-test", fixed)
	if _, err := unknownManager.ParseAccess(token); err == nil {
		t.Fatal("token signed by unknown key was accepted")
	}
}

func TestJWTExpiration(t *testing.T) {
	fixed := clock.NewFixed(time.Date(2026, time.November, 2, 12, 0, 0, 0, time.UTC))
	manager, _ := NewJWTManager(testKey("current"), nil, "harness-test", fixed)
	token, err := manager.IssueAccess(uuid.NewString(), "admin", 2)
	if err != nil {
		t.Fatal(err)
	}
	fixed.Advance(AccessTokenTTL + time.Second)
	if _, err := manager.ParseAccess(token); err == nil {
		t.Fatal("expired token was accepted")
	}
}
