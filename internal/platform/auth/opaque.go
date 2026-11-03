package auth

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
)

func NewOpaqueToken() (string, error) {
	value := make([]byte, 32)
	if _, err := rand.Read(value); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(value), nil
}

func HashOpaqueToken(token string, pepper []byte) ([]byte, error) {
	if token == "" || len(pepper) < 32 {
		return nil, errors.New("opaque token and a 32-byte pepper are required")
	}
	hash := hmac.New(sha256.New, pepper)
	_, _ = hash.Write([]byte(token))
	return hash.Sum(nil), nil
}
