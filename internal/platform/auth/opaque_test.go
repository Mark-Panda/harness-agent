package auth

import (
	"bytes"
	"testing"
)

func TestOpaqueTokenAndHash(t *testing.T) {
	first, err := NewOpaqueToken()
	if err != nil {
		t.Fatal(err)
	}
	second, err := NewOpaqueToken()
	if err != nil {
		t.Fatal(err)
	}
	if first == second {
		t.Fatal("opaque tokens must be random")
	}
	pepper := []byte("01234567890123456789012345678901")
	firstHash, err := HashOpaqueToken(first, pepper)
	if err != nil {
		t.Fatal(err)
	}
	secondHash, _ := HashOpaqueToken(first, pepper)
	if !bytes.Equal(firstHash, secondHash) || len(firstHash) != 32 {
		t.Fatal("opaque token hash is not deterministic")
	}
	differentHash, _ := HashOpaqueToken(first, []byte("12345678901234567890123456789012"))
	if bytes.Equal(firstHash, differentHash) {
		t.Fatal("different peppers produced the same hash")
	}
}
