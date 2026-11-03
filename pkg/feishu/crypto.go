package feishu

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
)

// DecryptPayload 使用 AES-256-CBC 解密飞书事件密文。
// 关键步骤：密钥由 Encrypt Key 经 SHA-256 派生；密文前 16 字节为 IV，其余为 PKCS#7 填充的密文块。
func DecryptPayload(encryptKey, ciphertext string) ([]byte, error) {
	raw, err := base64.StdEncoding.DecodeString(ciphertext)
	if err != nil {
		return nil, fmt.Errorf("decode feishu ciphertext: %w", err)
	}
	if len(raw) < aes.BlockSize {
		return nil, errors.New("feishu ciphertext is shorter than the IV")
	}
	key := sha256.Sum256([]byte(encryptKey))
	block, err := aes.NewCipher(key[:])
	if err != nil {
		return nil, fmt.Errorf("create feishu cipher: %w", err)
	}
	iv := raw[:aes.BlockSize]
	payload := raw[aes.BlockSize:]
	if len(payload) == 0 || len(payload)%aes.BlockSize != 0 {
		return nil, errors.New("feishu ciphertext is not block aligned")
	}
	plaintext := make([]byte, len(payload))
	cipher.NewCBCDecrypter(block, iv).CryptBlocks(plaintext, payload)
	return unpadPKCS7(plaintext)
}

// encryptPayloadForTest 仅用于测试构造飞书密文，保持与 DecryptPayload 对称。
func encryptPayloadForTest(encryptKey, plaintext string) (string, error) {
	key := sha256.Sum256([]byte(encryptKey))
	block, err := aes.NewCipher(key[:])
	if err != nil {
		return "", err
	}
	iv := bytes.Repeat([]byte{0x42}, aes.BlockSize)
	padded := padPKCS7([]byte(plaintext), aes.BlockSize)
	out := make([]byte, aes.BlockSize+len(padded))
	copy(out, iv)
	cipher.NewCBCEncrypter(block, iv).CryptBlocks(out[aes.BlockSize:], padded) // #nosec G407 -- 仅测试辅助构造，生产解密从密文读取随机 IV
	return base64.StdEncoding.EncodeToString(out), nil
}

// padPKCS7 按 PKCS#7 规范补齐块长度。
func padPKCS7(data []byte, blockSize int) []byte {
	padding := blockSize - len(data)%blockSize
	return append(data, bytes.Repeat([]byte{byte(padding)}, padding)...)
}

// unpadPKCS7 校验并移除 PKCS#7 填充。
func unpadPKCS7(data []byte) ([]byte, error) {
	if len(data) == 0 {
		return nil, errors.New("feishu plaintext is empty")
	}
	padding := int(data[len(data)-1])
	if padding <= 0 || padding > aes.BlockSize || padding > len(data) {
		return nil, errors.New("invalid feishu padding")
	}
	for _, value := range data[len(data)-padding:] {
		if int(value) != padding {
			return nil, errors.New("invalid feishu padding bytes")
		}
	}
	return data[:len(data)-padding], nil
}
