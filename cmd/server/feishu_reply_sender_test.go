package main

import (
	"context"
	"testing"

	"github.com/Mark-Panda/harness-agent/internal/biz"
	"github.com/google/uuid"
)

type testFeishuBots struct {
	bot    biz.FeishuBot
	secret string
}

func (s *testFeishuBots) GetFeishuBotCredential(_ context.Context, id uuid.UUID) (biz.FeishuBot, biz.ProviderCredential, error) {
	if id != s.bot.ID || id == uuid.Nil {
		return biz.FeishuBot{}, biz.ProviderCredential{}, biz.ErrResourceNotFound
	}
	return s.bot, biz.ProviderCredential{KeyVersion: "v1", Encrypted: []byte(`{"app_secret":"` + s.secret + `"}`)}, nil
}

type testFeishuOpener struct{}

func (testFeishuOpener) Open(_ string, ciphertext []byte) ([]byte, error) { return ciphertext, nil }

func TestFeishuReplySenderValidatesCachedRoute(t *testing.T) {
	ctx := context.Background()
	bot := biz.FeishuBot{ID: uuid.New(), AppID: "cli_registered", Status: biz.FeishuBotEnabled}
	for _, scenario := range []string{"disabled", "deleted", "recreated", "changed app", "rotated"} {
		t.Run(scenario, func(t *testing.T) {
			bots := &testFeishuBots{bot: bot, secret: "original"}
			resolver := &feishuBotSenderResolver{credentials: bots, opener: testFeishuOpener{}}
			original, err := resolver.ResolveSender(ctx, bot.ID, bot.AppID)
			if err != nil || original == nil {
				t.Fatalf("initial sender = %v, %v", original, err)
			}
			if cached, err := resolver.ResolveSender(ctx, bot.ID, bot.AppID); err != nil || cached != original {
				t.Fatal("unchanged credential did not reuse sender")
			}
			switch scenario {
			case "disabled":
				bots.bot.Status = biz.FeishuBotDisabled
			case "deleted":
				bots.bot = biz.FeishuBot{}
			case "recreated":
				bots.bot.ID = uuid.New()
			case "changed app":
				bots.bot.AppID = "cli_changed"
			case "rotated":
				bots.secret = "rotated"
			}
			resolved, err := resolver.ResolveSender(ctx, bot.ID, bot.AppID)
			if scenario == "rotated" {
				if err != nil || resolved == nil || resolved == original {
					t.Fatal("rotation did not replace cached sender")
				}
			} else if err == nil || resolved != nil {
				t.Fatal("invalid bot route resolved to a sender")
			}
		})
	}
}

func TestFeishuReplySenderRejectsUnknownRoute(t *testing.T) {
	bots := &testFeishuBots{}
	resolver := &feishuBotSenderResolver{credentials: bots, opener: testFeishuOpener{}}
	if sender, err := resolver.ResolveSender(context.Background(), uuid.New(), "cli_unknown"); err == nil || sender != nil {
		t.Fatal("unknown app resolved to a sender")
	}
}
