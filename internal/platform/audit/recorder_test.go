package audit

import (
	"encoding/json"
	"testing"
)

func TestSanitizeMetadataDropsSecretsAndNestedContent(t *testing.T) {
	metadata, err := sanitizeMetadata(map[string]any{
		"provider": "openai",
		"result":   "success",
		"password": "secret",
		"tool":     "echo",
		"nested":   map[string]any{"private": true},
	})
	if err != nil {
		t.Fatal(err)
	}
	var values map[string]any
	if err := json.Unmarshal(metadata, &values); err != nil {
		t.Fatal(err)
	}
	if values["provider"] != "openai" || values["result"] != "success" || values["tool"] != "echo" {
		t.Fatalf("allowed metadata missing: %#v", values)
	}
	if _, ok := values["password"]; ok {
		t.Fatal("secret metadata was retained")
	}
	if _, ok := values["nested"]; ok {
		t.Fatal("nested content was retained")
	}
}

func TestPseudonymUsesKeyedHash(t *testing.T) {
	recorder := &Recorder{key: []byte("01234567890123456789012345678901")}
	first := recorder.pseudonym("user-1")
	second := recorder.pseudonym("user-1")
	if first == "user-1" || first != second {
		t.Fatal("pseudonym must be deterministic and keyed")
	}
}
