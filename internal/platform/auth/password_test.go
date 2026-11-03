package auth

import "testing"

func TestPasswordHashAndVerify(t *testing.T) {
	const password = "correct horse battery"
	first, err := HashPassword(password)
	if err != nil {
		t.Fatal(err)
	}
	second, err := HashPassword(password)
	if err != nil {
		t.Fatal(err)
	}
	if first == second {
		t.Fatal("password salts must be random")
	}
	valid, err := VerifyPassword(first, password)
	if err != nil || !valid {
		t.Fatalf("VerifyPassword() = %t, %v", valid, err)
	}
	valid, err = VerifyPassword(first, "incorrect password")
	if err != nil || valid {
		t.Fatalf("wrong password = %t, %v", valid, err)
	}
	if _, err := HashPassword("short"); err != ErrWeakPassword {
		t.Fatalf("weak password error = %v", err)
	}
	if _, err := VerifyPassword("not-a-hash", password); err == nil {
		t.Fatal("malformed password hash was accepted")
	}
}
