package feishu

import "testing"

// FuzzDecryptPayload 验证任意密钥/密文组合都不会触发解密 panic 或越界。
func FuzzDecryptPayload(f *testing.F) {
	valid, _ := encryptPayloadForTest("test-key", `{"schema":"2.0"}`)
	f.Add("test-key", valid)
	f.Add("test-key", "AAAA")
	f.Add("", "")
	f.Fuzz(func(t *testing.T, encryptKey, ciphertext string) {
		if _, err := DecryptPayload(encryptKey, ciphertext); err != nil {
			// 非法输入返回错误是预期行为，只需保证不 panic。
			return
		}
	})
}

// FuzzDecodeEnvelope 验证任意事件 JSON 都不会触发解析 panic。
func FuzzDecodeEnvelope(f *testing.F) {
	f.Add([]byte(`{"encrypt":"","schema":"2.0"}`))
	f.Add([]byte(`{"schema":"1.0","event":{}}`))
	f.Add([]byte(`not-json`))
	f.Fuzz(func(t *testing.T, body []byte) {
		_, _ = DecodeEnvelope(body)
	})
}
