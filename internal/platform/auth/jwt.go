package auth

import (
	"errors"
	"fmt"
	"time"

	"github.com/Mark-Panda/harness-agent/internal/platform/clock"
	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
)

const AccessTokenTTL = 15 * time.Minute

var ErrInvalidAccessToken = errors.New("invalid access token")

type SigningKey struct {
	ID     string
	Secret []byte
}

type AccessClaims struct {
	Role              string `json:"role"`
	CredentialVersion int64  `json:"credential_version"`
	TokenType         string `json:"token_type"`
	jwt.RegisteredClaims
}

type JWTManager struct {
	signingKey SigningKey
	keys       map[string][]byte
	issuer     string
	clock      clock.Clock
}

func NewJWTManager(signingKey SigningKey, verificationKeys []SigningKey, issuer string, timeSource clock.Clock) (*JWTManager, error) {
	if signingKey.ID == "" || len(signingKey.Secret) < 32 || issuer == "" {
		return nil, errors.New("jwt signing key, key id and issuer are required")
	}
	if timeSource == nil {
		timeSource = clock.System{}
	}
	keys := make(map[string][]byte, len(verificationKeys)+1)
	for _, key := range append(verificationKeys, signingKey) {
		if key.ID == "" || len(key.Secret) < 32 {
			return nil, errors.New("invalid jwt verification key")
		}
		keys[key.ID] = key.Secret
	}
	return &JWTManager{signingKey: signingKey, keys: keys, issuer: issuer, clock: timeSource}, nil
}

func (m *JWTManager) IssueAccess(userID, role string, credentialVersion int64) (string, error) {
	if userID == "" || (role != "user" && role != "admin") || credentialVersion <= 0 {
		return "", errors.New("invalid access token subject")
	}
	now := m.clock.Now()
	claims := AccessClaims{
		Role:              role,
		CredentialVersion: credentialVersion,
		TokenType:         "access",
		RegisteredClaims: jwt.RegisteredClaims{
			ID:        uuid.NewString(),
			Issuer:    m.issuer,
			Subject:   userID,
			IssuedAt:  jwt.NewNumericDate(now),
			NotBefore: jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(now.Add(AccessTokenTTL)),
		},
	}
	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	token.Header["kid"] = m.signingKey.ID
	return token.SignedString(m.signingKey.Secret)
}

func (m *JWTManager) ParseAccess(tokenString string) (*AccessClaims, error) {
	claims := &AccessClaims{}
	token, err := jwt.ParseWithClaims(tokenString, claims, func(token *jwt.Token) (any, error) {
		keyID, ok := token.Header["kid"].(string)
		if !ok {
			return nil, ErrInvalidAccessToken
		}
		secret, ok := m.keys[keyID]
		if !ok {
			return nil, ErrInvalidAccessToken
		}
		return secret, nil
	}, jwt.WithValidMethods([]string{jwt.SigningMethodHS256.Alg()}), jwt.WithIssuer(m.issuer), jwt.WithExpirationRequired(), jwt.WithIssuedAt(), jwt.WithTimeFunc(m.clock.Now))
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidAccessToken, err)
	}
	if !token.Valid || claims.TokenType != "access" || claims.Subject == "" || claims.CredentialVersion <= 0 {
		return nil, ErrInvalidAccessToken
	}
	if claims.Role != "user" && claims.Role != "admin" {
		return nil, ErrInvalidAccessToken
	}
	return claims, nil
}

func (m *JWTManager) SigningKeyID() string {
	return m.signingKey.ID
}
