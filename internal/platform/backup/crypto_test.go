package backup

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// writeKeyFile 写入 base64 编码的 32 字节密钥。
func writeKeyFile(t *testing.T) (string, []byte) {
	t.Helper()
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "backup.key")
	if err := os.WriteFile(path, []byte(base64.StdEncoding.EncodeToString(key)), 0o600); err != nil {
		t.Fatal(err)
	}
	return path, key
}

// TestLoadKeyFileSupportsBase64AndHex 验证密钥文件编码兼容与非法密钥拒绝。
func TestLoadKeyFileSupportsBase64AndHex(t *testing.T) {
	_, key := writeKeyFile(t)
	base64Path := filepath.Join(t.TempDir(), "base64.key")
	if err := os.WriteFile(base64Path, []byte(base64.StdEncoding.EncodeToString(key)), 0o600); err != nil {
		t.Fatal(err)
	}
	if loaded, err := LoadKeyFile(base64Path); err != nil || !bytes.Equal(loaded, key) {
		t.Fatalf("base64 key load = %x, %v", loaded, err)
	}
	hexPath := filepath.Join(t.TempDir(), "hex.key")
	if err := os.WriteFile(hexPath, []byte(hex.EncodeToString(key)), 0o600); err != nil {
		t.Fatal(err)
	}
	if loaded, err := LoadKeyFile(hexPath); err != nil || !bytes.Equal(loaded, key) {
		t.Fatalf("hex key load = %x, %v", loaded, err)
	}
	for name, content := range map[string]string{"short": base64.StdEncoding.EncodeToString(key[:16]), "garbage": "not-a-key"} {
		path := filepath.Join(t.TempDir(), name)
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadKeyFile(path); err == nil {
			t.Fatalf("%s key must be rejected", name)
		}
	}
}

// TestEncryptDecryptRoundTripAcrossChunkBoundaries 验证空文件与跨块边界的加解密一致性。
func TestEncryptDecryptRoundTripAcrossChunkBoundaries(t *testing.T) {
	_, key := writeKeyFile(t)
	for _, size := range []int{0, 1, 31, 32, 1024, DefaultChunkSize - 1, DefaultChunkSize, DefaultChunkSize + 1, 2 * DefaultChunkSize} {
		plaintext := make([]byte, size)
		if _, err := rand.Read(plaintext); err != nil {
			t.Fatal(err)
		}
		var encrypted bytes.Buffer
		if err := Encrypt(&encrypted, bytes.NewReader(plaintext), key, DefaultChunkSize); err != nil {
			t.Fatalf("encrypt size %d: %v", size, err)
		}
		var decrypted bytes.Buffer
		if err := Decrypt(&decrypted, bytes.NewReader(encrypted.Bytes()), key); err != nil {
			t.Fatalf("decrypt size %d: %v", size, err)
		}
		if !bytes.Equal(decrypted.Bytes(), plaintext) {
			t.Fatalf("roundtrip size %d mismatch", size)
		}
	}
}

// TestDecryptRejectsTamperedTruncatedAndWrongKey 验证篡改、截断与错误密钥均被拒绝。
func TestDecryptRejectsTamperedTruncatedAndWrongKey(t *testing.T) {
	_, key := writeKeyFile(t)
	plaintext := []byte("sensitive backup payload")
	var encrypted bytes.Buffer
	if err := Encrypt(&encrypted, bytes.NewReader(plaintext), key, DefaultChunkSize); err != nil {
		t.Fatal(err)
	}
	original := encrypted.Bytes()

	tampered := append([]byte(nil), original...)
	tampered[len(tampered)-1] ^= 0xFF
	if err := Decrypt(&bytes.Buffer{}, bytes.NewReader(tampered), key); !errors.Is(err, ErrInvalidBackupFormat) {
		t.Fatalf("tampered decrypt error = %v", err)
	}
	if err := Decrypt(&bytes.Buffer{}, bytes.NewReader(original[:len(original)-5]), key); !errors.Is(err, ErrInvalidBackupFormat) {
		t.Fatalf("truncated decrypt error = %v", err)
	}
	wrongKey := append([]byte(nil), key...)
	wrongKey[0] ^= 0xFF
	if err := Decrypt(&bytes.Buffer{}, bytes.NewReader(original), wrongKey); !errors.Is(err, ErrInvalidBackupFormat) {
		t.Fatalf("wrong key decrypt error = %v", err)
	}
	if err := Decrypt(&bytes.Buffer{}, bytes.NewReader([]byte("not-a-backup")), key); !errors.Is(err, ErrInvalidBackupFormat) {
		t.Fatalf("invalid magic error = %v", err)
	}
	// 解密失败不得留下部分明文。
	var dst bytes.Buffer
	if err := Decrypt(&dst, bytes.NewReader(tampered), key); err == nil || dst.Len() != 0 {
		t.Fatalf("failed decrypt wrote %d bytes", dst.Len())
	}
}
