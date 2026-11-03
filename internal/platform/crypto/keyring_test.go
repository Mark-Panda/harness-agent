package crypto

import (
	"bytes"
	"errors"
	"strings"
	"testing"
	"testing/iotest"
)

var (
	activeKey = Key{ID: "active", Secret: bytes.Repeat([]byte{0x11}, keySize)}
	oldKey    = Key{ID: "old", Secret: bytes.Repeat([]byte{0x22}, keySize)}
)

func TestNewKeyring(t *testing.T) {
	tests := []struct {
		name         string
		active       Key
		previousKeys []Key
		wantErr      string
	}{
		{name: "active only", active: activeKey},
		{name: "with previous key", active: activeKey, previousKeys: []Key{oldKey}},
		{name: "missing active id", active: Key{Secret: activeKey.Secret}, wantErr: "active key id"},
		{name: "blank active id", active: Key{ID: "  ", Secret: activeKey.Secret}, wantErr: "active key id"},
		{name: "short active key", active: Key{ID: "active", Secret: make([]byte, keySize-1)}, wantErr: "exactly 32 bytes"},
		{name: "long active key", active: Key{ID: "active", Secret: make([]byte, keySize+1)}, wantErr: "exactly 32 bytes"},
		{name: "missing previous id", active: activeKey, previousKeys: []Key{{Secret: oldKey.Secret}}, wantErr: "previous key id"},
		{name: "short previous key", active: activeKey, previousKeys: []Key{{ID: "old", Secret: make([]byte, keySize-1)}}, wantErr: "exactly 32 bytes"},
		{name: "previous overwrites active", active: activeKey, previousKeys: []Key{{ID: activeKey.ID, Secret: oldKey.Secret}}, wantErr: "duplicate key id"},
		{name: "duplicate previous ids", active: activeKey, previousKeys: []Key{oldKey, oldKey}, wantErr: "duplicate key id"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			keyring, err := NewKeyring(tt.active, tt.previousKeys)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("NewKeyring() error = %v, want error containing %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("NewKeyring() error = %v", err)
			}
			if keyring == nil {
				t.Fatal("NewKeyring() returned nil keyring")
			}
		})
	}
}

func TestKeyringSealOpen(t *testing.T) {
	tests := []struct {
		name      string
		plaintext []byte
	}{
		{name: "text", plaintext: []byte("provider api key")},
		{name: "unicode", plaintext: []byte("凭证内容")},
		{name: "binary", plaintext: []byte{0x00, 0xff, 0x10, 0x00, 0x80}},
		{name: "large", plaintext: bytes.Repeat([]byte("secret"), 1024)},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			keyring, err := NewKeyring(activeKey, nil)
			if err != nil {
				t.Fatal(err)
			}
			ciphertext, keyVersion, err := keyring.Seal(tt.plaintext)
			if err != nil {
				t.Fatalf("Seal() error = %v", err)
			}
			if keyVersion != activeKey.ID {
				t.Fatalf("Seal() key version = %q, want %q", keyVersion, activeKey.ID)
			}
			plaintext, err := keyring.Open(keyVersion, ciphertext)
			if err != nil {
				t.Fatalf("Open() error = %v", err)
			}
			if !bytes.Equal(plaintext, tt.plaintext) {
				t.Fatalf("Open() plaintext = %x, want %x", plaintext, tt.plaintext)
			}
		})
	}
}

func TestKeyringSealUsesRandomNonce(t *testing.T) {
	keyring, err := NewKeyring(activeKey, nil)
	if err != nil {
		t.Fatal(err)
	}
	plaintext := []byte("same plaintext")
	first, _, err := keyring.Seal(plaintext)
	if err != nil {
		t.Fatal(err)
	}
	second, _, err := keyring.Seal(plaintext)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(first, second) {
		t.Fatal("Seal() produced identical ciphertexts for identical plaintexts")
	}
}

func TestKeyringOpensCiphertextsAfterRotation(t *testing.T) {
	newKey := Key{ID: "new", Secret: bytes.Repeat([]byte{0x33}, keySize)}
	olderKey := Key{ID: "older", Secret: bytes.Repeat([]byte{0x44}, keySize)}
	keyVersions := []Key{oldKey, olderKey}
	type encrypted struct {
		keyVersion string
		ciphertext []byte
		plaintext  []byte
	}
	var historical []encrypted
	for _, key := range keyVersions {
		keyring, err := NewKeyring(key, nil)
		if err != nil {
			t.Fatal(err)
		}
		plaintext := []byte("secret from " + key.ID)
		ciphertext, keyVersion, err := keyring.Seal(plaintext)
		if err != nil {
			t.Fatal(err)
		}
		historical = append(historical, encrypted{keyVersion: keyVersion, ciphertext: ciphertext, plaintext: plaintext})
	}

	rotated, err := NewKeyring(newKey, keyVersions)
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range historical {
		t.Run(item.keyVersion, func(t *testing.T) {
			plaintext, err := rotated.Open(item.keyVersion, item.ciphertext)
			if err != nil {
				t.Fatalf("Open() error = %v", err)
			}
			if !bytes.Equal(plaintext, item.plaintext) {
				t.Fatalf("Open() plaintext = %q, want %q", plaintext, item.plaintext)
			}
		})
	}

	ciphertext, keyVersion, err := rotated.Seal([]byte("new secret"))
	if err != nil {
		t.Fatal(err)
	}
	if keyVersion != newKey.ID {
		t.Fatalf("Seal() key version = %q, want %q", keyVersion, newKey.ID)
	}
	if _, err := rotated.Open(keyVersion, ciphertext); err != nil {
		t.Fatalf("Open() new ciphertext error = %v", err)
	}
}

func TestKeyringSealRejectsEmptyPlaintext(t *testing.T) {
	keyring, err := NewKeyring(activeKey, nil)
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name      string
		plaintext []byte
	}{
		{name: "nil", plaintext: nil},
		{name: "empty", plaintext: []byte{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ciphertext, keyVersion, err := keyring.Seal(tt.plaintext)
			if err == nil || !strings.Contains(err.Error(), "plaintext must not be empty") {
				t.Fatalf("Seal() error = %v, want empty plaintext error", err)
			}
			if ciphertext != nil || keyVersion != "" {
				t.Fatalf("Seal() returned ciphertext %x and version %q on error", ciphertext, keyVersion)
			}
		})
	}
}

func TestKeyringOpenRejectsInvalidCiphertext(t *testing.T) {
	keyring, err := NewKeyring(activeKey, nil)
	if err != nil {
		t.Fatal(err)
	}
	ciphertext, _, err := keyring.Seal([]byte("authenticated plaintext"))
	if err != nil {
		t.Fatal(err)
	}
	nonceSize := keyring.active.NonceSize()
	tests := []struct {
		name       string
		keyVersion string
		mutate     func([]byte) []byte
	}{
		{
			name:       "unknown version",
			keyVersion: "missing",
			mutate:     func(value []byte) []byte { return value },
		},
		{
			name:   "empty version",
			mutate: func(value []byte) []byte { return value },
		},
		{
			name:   "empty ciphertext",
			mutate: func([]byte) []byte { return nil },
		},
		{
			name:   "short ciphertext",
			mutate: func(value []byte) []byte { return value[:nonceSize] },
		},
		{
			name: "tampered nonce",
			mutate: func(value []byte) []byte {
				value[0] ^= 0xff
				return value
			},
		},
		{
			name: "tampered body",
			mutate: func(value []byte) []byte {
				value[nonceSize] ^= 0xff
				return value
			},
		},
		{
			name: "tampered tag",
			mutate: func(value []byte) []byte {
				value[len(value)-1] ^= 0xff
				return value
			},
		},
		{
			name: "truncated tag",
			mutate: func(value []byte) []byte {
				return value[:len(value)-1]
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			keyVersion := tt.keyVersion
			if keyVersion == "" && tt.name != "empty version" {
				keyVersion = activeKey.ID
			}
			value := tt.mutate(bytes.Clone(ciphertext))
			plaintext, err := keyring.Open(keyVersion, value)
			if err == nil {
				t.Fatalf("Open() error = nil, plaintext = %q", plaintext)
			}
			if plaintext != nil {
				t.Fatalf("Open() returned plaintext %q on error", plaintext)
			}
		})
	}
}

func TestKeyringSealReportsRandomSourceFailure(t *testing.T) {
	keyring, err := NewKeyring(activeKey, nil)
	if err != nil {
		t.Fatal(err)
	}
	keyring.random = iotest.ErrReader(errors.New("random source unavailable"))
	ciphertext, keyVersion, err := keyring.Seal([]byte("secret"))
	if err == nil || !strings.Contains(err.Error(), "generate nonce") {
		t.Fatalf("Seal() error = %v, want nonce generation error", err)
	}
	if ciphertext != nil || keyVersion != "" {
		t.Fatalf("Seal() returned ciphertext %x and version %q on error", ciphertext, keyVersion)
	}
}
