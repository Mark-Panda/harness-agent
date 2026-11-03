package backup

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
)

// 备份加密格式：文件头 + 分块 AES-256-GCM。
// 约束：密钥只从文件读取，禁止出现在命令行、日志或 Manifest 中。
const (
	// magic 是加密文件头标识，用于快速拒绝非备份文件。
	magic = "HBK1"
	// DefaultChunkSize 是单块明文大小，限制内存占用并支持大备份流式加解密。
	DefaultChunkSize = 1 << 20
	// nonceSize 是 AES-GCM 标准 nonce 长度。
	nonceSize = 12
)

// ErrInvalidBackupFormat 表示输入不是本系统生成的加密备份。
var ErrInvalidBackupFormat = errors.New("invalid backup encryption format")

// LoadKeyFile 从文件加载备份密钥。
// 支持 base64（标准或 URL 编码）或 hex 编码的 32 字节密钥，文件首尾空白会被忽略。
func LoadKeyFile(path string) ([]byte, error) {
	if strings.TrimSpace(path) == "" {
		return nil, errors.New("backup key file is required")
	}
	raw, err := os.ReadFile(path) // #nosec G304 -- 密钥文件路径由运维命令显式指定
	if err != nil {
		return nil, fmt.Errorf("read backup key: %w", err)
	}
	encoded := strings.TrimSpace(string(raw))
	if decoded, decodeErr := base64.StdEncoding.DecodeString(encoded); decodeErr == nil && len(decoded) == 32 {
		return decoded, nil
	}
	if decoded, decodeErr := base64.RawURLEncoding.DecodeString(encoded); decodeErr == nil && len(decoded) == 32 {
		return decoded, nil
	}
	if decoded, decodeErr := hex.DecodeString(encoded); decodeErr == nil && len(decoded) == 32 {
		return decoded, nil
	}
	return nil, errors.New("backup key must be 32 bytes encoded as base64 or hex")
}

// Encrypt 用 AES-256-GCM 流式加密。
// 关键步骤：每块使用随机 nonce，并把块序号作为 AAD，防止块被重排或替换。
func Encrypt(dst io.Writer, src io.Reader, key []byte, chunkSize int) error {
	aead, err := newAEAD(key)
	if err != nil {
		return err
	}
	if chunkSize <= 0 || chunkSize > 16*DefaultChunkSize {
		chunkSize = DefaultChunkSize
	}
	header := make([]byte, 0, 8)
	header = append(header, magic...)
	header = binary.BigEndian.AppendUint32(header, uint32(chunkSize)) // #nosec G115 -- chunkSize 已限制在 16 MiB 以内
	if _, err := dst.Write(header); err != nil {
		return fmt.Errorf("write backup header: %w", err)
	}
	buffer := make([]byte, chunkSize)
	for index := uint32(0); ; index++ {
		n, readErr := io.ReadFull(src, buffer)
		if readErr == io.EOF {
			return nil
		}
		if readErr != nil && readErr != io.ErrUnexpectedEOF {
			return fmt.Errorf("read backup source: %w", readErr)
		}
		nonce := make([]byte, nonceSize)
		if _, err := rand.Read(nonce); err != nil {
			return fmt.Errorf("generate backup nonce: %w", err)
		}
		sealed := aead.Seal(nil, nonce, buffer[:n], chunkAAD(index, chunkSize)) // #nosec G407 -- nonce 由 crypto/rand 随机生成
		record := make([]byte, 0, 4+nonceSize+len(sealed))
		record = binary.BigEndian.AppendUint32(record, uint32(n)) // #nosec G115 -- n 不超过已校验的 chunkSize
		record = append(record, nonce...)
		record = append(record, sealed...)
		if _, err := dst.Write(record); err != nil {
			return fmt.Errorf("write backup chunk: %w", err)
		}
		if readErr == io.ErrUnexpectedEOF {
			return nil
		}
	}
}

// Decrypt 校验并解密备份流。
// 关键步骤：任何格式错误、密钥错误或篡改都返回错误，禁止输出部分明文后静默成功。
func Decrypt(dst io.Writer, src io.Reader, key []byte) error {
	aead, err := newAEAD(key)
	if err != nil {
		return err
	}
	header := make([]byte, 8)
	if _, err := io.ReadFull(src, header); err != nil {
		return fmt.Errorf("%w: read header: %v", ErrInvalidBackupFormat, err)
	}
	if string(header[:4]) != magic {
		return fmt.Errorf("%w: unknown magic", ErrInvalidBackupFormat)
	}
	chunkSize := binary.BigEndian.Uint32(header[4:])
	if chunkSize == 0 || chunkSize > 16*DefaultChunkSize {
		return fmt.Errorf("%w: invalid chunk size %d", ErrInvalidBackupFormat, chunkSize)
	}
	for index := uint32(0); ; index++ {
		lengthBytes := make([]byte, 4)
		_, err := io.ReadFull(src, lengthBytes)
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return fmt.Errorf("%w: read chunk length: %v", ErrInvalidBackupFormat, err)
		}
		plainLen := binary.BigEndian.Uint32(lengthBytes)
		if plainLen > chunkSize {
			return fmt.Errorf("%w: chunk %d exceeds size", ErrInvalidBackupFormat, index)
		}
		nonce := make([]byte, nonceSize)
		if _, err := io.ReadFull(src, nonce); err != nil {
			return fmt.Errorf("%w: read nonce: %v", ErrInvalidBackupFormat, err)
		}
		sealed := make([]byte, int(plainLen)+aead.Overhead())
		if _, err := io.ReadFull(src, sealed); err != nil {
			return fmt.Errorf("%w: read chunk payload: %v", ErrInvalidBackupFormat, err)
		}
		plain, err := aead.Open(nil, nonce, sealed, chunkAAD(index, int(chunkSize)))
		if err != nil {
			return fmt.Errorf("%w: decrypt chunk %d: %v", ErrInvalidBackupFormat, index, err)
		}
		if _, err := dst.Write(plain); err != nil {
			return fmt.Errorf("write decrypted chunk: %w", err)
		}
	}
}

// newAEAD 由 32 字节密钥构造 AES-256-GCM。
func newAEAD(key []byte) (cipher.AEAD, error) {
	if len(key) != 32 {
		return nil, errors.New("AES-256 requires a 32-byte key")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("create backup cipher: %w", err)
	}
	return cipher.NewGCM(block)
}

// chunkAAD 把块序号与块大小绑定到认证数据，阻止块重排攻击。
func chunkAAD(index uint32, chunkSize int) []byte {
	aad := make([]byte, 0, 8)
	aad = binary.BigEndian.AppendUint32(aad, index)
	aad = binary.BigEndian.AppendUint32(aad, uint32(chunkSize)) // #nosec G115 -- chunkSize 已由调用方限制
	return aad
}
