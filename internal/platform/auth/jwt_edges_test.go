package auth

import (
	"testing"
	"time"

	"github.com/Mark-Panda/harness-agent/internal/platform/clock"
	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
)

func TestJWTManagerValidation(t *testing.T) {
	if _, err := NewJWTManager(SigningKey{}, nil, "", nil); err == nil {
		t.Fatal("empty signing configuration was accepted")
	}
	manager, err := NewJWTManager(testKey("current"), []SigningKey{testKey("old")}, "harness-test", nil)
	if err != nil {
		t.Fatal(err)
	}
	if manager.SigningKeyID() != "current" {
		t.Fatalf("SigningKeyID() = %q", manager.SigningKeyID())
	}
	if _, err := NewJWTManager(testKey("current"), []SigningKey{{ID: "bad", Secret: []byte("short")}}, "harness-test", nil); err == nil {
		t.Fatal("short verification key was accepted")
	}
	if _, err := manager.IssueAccess("", "user", 1); err == nil {
		t.Fatal("empty subject was accepted")
	}
	if _, err := manager.IssueAccess(uuid.NewString(), "invalid", 1); err == nil {
		t.Fatal("invalid role was accepted")
	}
	if _, err := manager.IssueAccess(uuid.NewString(), "user", 0); err == nil {
		t.Fatal("invalid credential version was accepted")
	}
}

func TestJWTIssuerValidation(t *testing.T) {
	first, _ := NewJWTManager(testKey("current"), nil, "issuer-a", nil)
	second, _ := NewJWTManager(testKey("current"), nil, "issuer-b", nil)
	token, err := first.IssueAccess(uuid.NewString(), "user", 1)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := second.ParseAccess(token); err == nil {
		t.Fatal("token from another issuer was accepted")
	}
}

// TestJWTParsedClaimsValidation 验证令牌类型、角色与凭据版本的非法声明被拒绝。
func TestJWTParsedClaimsValidation(t *testing.T) {
	now := time.Date(2026, time.November, 3, 12, 0, 0, 0, time.UTC)
	fixed := clock.NewFixed(now)
	manager, err := NewJWTManager(testKey("current"), nil, "harness-test", fixed)
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name   string
		mutate func(*AccessClaims)
	}{
		{"token type", func(claims *AccessClaims) { claims.TokenType = "refresh" }},
		{"role", func(claims *AccessClaims) { claims.Role = "guest" }},
		{"credential version", func(claims *AccessClaims) { claims.CredentialVersion = 0 }},
		{"subject", func(claims *AccessClaims) { claims.Subject = "" }},
	}
	for _, testCase := range cases {
		claims := AccessClaims{
			Role: "user", CredentialVersion: 1, TokenType: "access",
			RegisteredClaims: jwt.RegisteredClaims{
				Issuer: "harness-test", Subject: uuid.NewString(),
				IssuedAt: jwt.NewNumericDate(now), NotBefore: jwt.NewNumericDate(now),
				ExpiresAt: jwt.NewNumericDate(now.Add(time.Minute)),
			},
		}
		testCase.mutate(&claims)
		token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
		token.Header["kid"] = "current"
		signed, err := token.SignedString(testKey("current").Secret)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := manager.ParseAccess(signed); err == nil {
			t.Fatalf("%s claims were accepted", testCase.name)
		}
	}
}
