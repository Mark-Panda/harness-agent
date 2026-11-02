package agent

import (
	"testing"

	"github.com/cloudwego/eino/schema"
)

func TestNewUserMessage(t *testing.T) {
	message := NewUserMessage("hello")
	if message.Role != schema.User || message.Content != "hello" {
		t.Fatalf("unexpected message: %#v", message)
	}
}
