package crypto

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"strings"
)

const keySize = 32

type Key struct {
	ID     string
	Secret []byte
}

type Keyring struct {
	activeKeyID string
	active      cipher.AEAD
	keys        map[string]cipher.AEAD
	random      io.Reader
}

func NewKeyring(activeKey Key, previousKeys []Key) (*Keyring, error) {
	if strings.TrimSpace(activeKey.ID) == "" {
		return nil, errors.New("active key id is required")
	}
	active, err := newGCM(activeKey.Secret)
	if err != nil {
		return nil, fmt.Errorf("invalid active key: %w", err)
	}

	keys := make(map[string]cipher.AEAD, len(previousKeys)+1)
	keys[activeKey.ID] = active
	for _, key := range previousKeys {
		if strings.TrimSpace(key.ID) == "" {
			return nil, errors.New("previous key id is required")
		}
		if _, exists := keys[key.ID]; exists {
			return nil, fmt.Errorf("duplicate key id %q", key.ID)
		}
		aead, err := newGCM(key.Secret)
		if err != nil {
			return nil, fmt.Errorf("invalid key %q: %w", key.ID, err)
		}
		keys[key.ID] = aead
	}

	return &Keyring{
		activeKeyID: activeKey.ID,
		active:      active,
		keys:        keys,
		random:      rand.Reader,
	}, nil
}

func (k *Keyring) Seal(plaintext []byte) (ciphertext []byte, keyVersion string, err error) {
	if len(plaintext) == 0 {
		return nil, "", errors.New("plaintext must not be empty")
	}
	nonce := make([]byte, k.active.NonceSize())
	if _, err := io.ReadFull(k.random, nonce); err != nil {
		return nil, "", fmt.Errorf("generate nonce: %w", err)
	}
	ciphertext = make([]byte, len(nonce), len(nonce)+len(plaintext)+k.active.Overhead())
	copy(ciphertext, nonce)
	ciphertext = k.active.Seal(ciphertext, nonce, plaintext, nil)
	return ciphertext, k.activeKeyID, nil
}

func (k *Keyring) Open(keyVersion string, ciphertext []byte) ([]byte, error) {
	aead, ok := k.keys[keyVersion]
	if !ok {
		return nil, fmt.Errorf("unknown key version %q", keyVersion)
	}
	if len(ciphertext) < aead.NonceSize()+aead.Overhead() {
		return nil, fmt.Errorf("ciphertext for key version %q is too short", keyVersion)
	}
	nonce := ciphertext[:aead.NonceSize()]
	sealed := ciphertext[aead.NonceSize():]
	plaintext, err := aead.Open(nil, nonce, sealed, nil)
	if err != nil {
		return nil, fmt.Errorf("decrypt ciphertext with key version %q: %w", keyVersion, err)
	}
	return plaintext, nil
}

func newGCM(secret []byte) (cipher.AEAD, error) {
	if len(secret) != keySize {
		return nil, fmt.Errorf("key must be exactly %d bytes", keySize)
	}
	block, err := aes.NewCipher(secret)
	if err != nil {
		return nil, fmt.Errorf("create AES cipher: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("create GCM: %w", err)
	}
	return aead, nil
}
