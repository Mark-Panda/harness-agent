package auth

import "testing"

func TestPasswordRejectsMalformedParameters(t *testing.T) {
	for _, encoded := range []string{
		"$argon2id$v=19$m=0,t=3,p=2$c2FsdA$aGFzaA",
		"$argon2id$v=18$m=65536,t=3,p=2$c2FsdA$aGFzaA",
		"$argon2id$v=19$m=65536,t=3,p=2$not-base64$aGFzaA",
	} {
		if _, err := VerifyPassword(encoded, "correct horse battery"); err == nil {
			t.Fatalf("malformed hash was accepted: %s", encoded)
		}
	}
}

func TestOpaqueTokenValidation(t *testing.T) {
	pepper := []byte("01234567890123456789012345678901")
	if _, err := HashOpaqueToken("", pepper); err == nil {
		t.Fatal("empty opaque token was accepted")
	}
	if _, err := HashOpaqueToken("token", []byte("short")); err == nil {
		t.Fatal("short pepper was accepted")
	}
}
