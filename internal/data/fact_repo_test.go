package data

import (
	"errors"
	"testing"

	"github.com/Mark-Panda/harness-agent/internal/biz"
	"github.com/google/uuid"
)

// TestValidateFactCorrectionSource 验证 Data 层 correction 不变量，不依赖数据库环境。
func TestValidateFactCorrectionSource(t *testing.T) {
	sourceID, userID, sessionID := uuid.New(), uuid.New(), uuid.New()
	newFact := func() biz.MemoryFact {
		return biz.MemoryFact{
			UserID: userID, SessionID: &sessionID, Corrected: true,
			SourceKind: biz.FactSourceCorrection, SourceMessageID: &sourceID,
		}
	}
	newMessages := func() []biz.Message {
		return []biz.Message{{ID: sourceID, UserID: userID, SessionID: sessionID, Role: biz.MessageUser, Content: "用户纠正"}}
	}
	for _, test := range []struct {
		name           string
		speaker        string
		mutateFact     func(*biz.MemoryFact)
		mutateMessages func(*[]biz.Message)
		wantErr        bool
	}{
		{name: "valid user"},
		{name: "valid speaker", speaker: "ou_a", mutateMessages: func(messages *[]biz.Message) { (*messages)[0].SpeakerOpenID = "ou_a" }},
		{name: "missing source id", mutateFact: func(fact *biz.MemoryFact) { fact.SourceMessageID = nil }, wantErr: true},
		{name: "nil source id", mutateFact: func(fact *biz.MemoryFact) { id := uuid.Nil; fact.SourceMessageID = &id }, wantErr: true},
		{name: "missing session", mutateFact: func(fact *biz.MemoryFact) { fact.SessionID = nil }, wantErr: true},
		{name: "source outside current set", mutateMessages: func(messages *[]biz.Message) { *messages = nil }, wantErr: true},
		{name: "assistant", mutateMessages: func(messages *[]biz.Message) { (*messages)[0].Role = biz.MessageAssistant }, wantErr: true},
		{name: "tool", mutateMessages: func(messages *[]biz.Message) { (*messages)[0].Role = biz.MessageTool }, wantErr: true},
		{name: "system", mutateMessages: func(messages *[]biz.Message) { (*messages)[0].Role = biz.MessageSystem }, wantErr: true},
		{name: "speaker mismatch", speaker: "ou_a", mutateMessages: func(messages *[]biz.Message) { (*messages)[0].SpeakerOpenID = "ou_b" }, wantErr: true},
		{name: "kind without corrected flag", mutateFact: func(fact *biz.MemoryFact) { fact.Corrected = false }, wantErr: true},
		{name: "corrected flag without kind", mutateFact: func(fact *biz.MemoryFact) { fact.SourceKind = biz.FactSourceUser }, wantErr: true},
		{name: "ordinary fact needs no correction source", mutateFact: func(fact *biz.MemoryFact) {
			fact.Corrected = false
			fact.SourceKind = biz.FactSourceAssistant
			fact.SourceMessageID = nil
			fact.SessionID = nil
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			fact, messages := newFact(), newMessages()
			if test.mutateFact != nil {
				test.mutateFact(&fact)
			}
			if test.mutateMessages != nil {
				test.mutateMessages(&messages)
			}
			err := validateFactCorrectionSource(fact, messages, test.speaker)
			if test.wantErr && !errors.Is(err, biz.ErrFactSourceInvalid) {
				t.Fatalf("error = %v, want ErrFactSourceInvalid", err)
			}
			if !test.wantErr && err != nil {
				t.Fatalf("unexpected error = %v", err)
			}
		})
	}
}
