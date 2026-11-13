package biz

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestRedactExecutionTextRemovesCredentialsFromOriginalBytes(t *testing.T) {
	for _, tc := range []struct {
		name, input, secret string
		structured          bool
	}{
		{"duplicate sensitive key", `{"password":"secret-value","password":"[REDACTED]","state":"visible"}`, "secret-value", true},
		{"duplicate text key", `{"message":"Bearer secret-value","message":"visible"}`, "secret-value", true},
		{"basic auth in text", "visible\nAuthorization: Basic dXNlcjpwYXNz", "dXNlcjpwYXNz", false},
		{"basic auth in nested text", `{"message":"visible\nAuthorization: Basic dXNlcjpwYXNz"}`, "dXNlcjpwYXNz", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := RedactExecutionText(tc.input)
			if strings.Contains(got, tc.secret) || !strings.Contains(got, "visible") || tc.structured && !json.Valid([]byte(got)) {
				t.Fatalf("audit redaction produced %q", got)
			}
		})
	}
}
