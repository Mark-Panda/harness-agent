package agent

import "github.com/cloudwego/eino/schema"

func NewUserMessage(content string) *schema.Message {
	return &schema.Message{Role: schema.User, Content: content}
}
