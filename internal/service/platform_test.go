package service

import (
	"testing"

	"github.com/Mark-Panda/harness-agent/internal/biz"
)

func TestNewPlatformService(t *testing.T) {
	providers := &biz.ProviderService{}
	models := &biz.ModelService{}
	policies := &biz.PolicyService{}
	skills := &biz.SkillService{}
	mcps := &biz.MCPService{}
	agents := &biz.AgentService{}
	sessions := &biz.SessionService{}
	runs := &biz.RunService{}
	idempotency := &biz.IdempotencyService{}
	platform, err := NewPlatformService(providers, models, policies, skills, mcps, agents, sessions, runs, idempotency)
	if err != nil {
		t.Fatal(err)
	}
	if platform.Providers != providers || platform.Models != models || platform.Policies != policies ||
		platform.Skills != skills || platform.MCPs != mcps || platform.Agents != agents || platform.Sessions != sessions ||
		platform.Runs != runs || platform.Idempotency != idempotency {
		t.Fatal("platform service dependencies were not preserved")
	}
	deletions := &biz.DeletionService{}
	exports := &biz.ExportService{}
	platform.WithDeletion(deletions, exports)
	if platform.Deletions != deletions || platform.Exports != exports {
		t.Fatal("deletion services were not attached")
	}
	plans := &biz.PlanService{}
	platform.WithPlans(plans)
	if platform.Plans != plans {
		t.Fatal("plan service was not attached")
	}
	routing := &biz.RoutingHealthService{}
	platform.WithRoutingHealth(routing)
	if platform.Routing != routing {
		t.Fatal("routing health service was not attached")
	}
	ingest := &biz.FeishuIngestService{}
	platform.WithFeishuIngest(ingest)
	if platform.FeishuIngest != ingest {
		t.Fatal("feishu ingest service was not attached")
	}
	bindings := &biz.FeishuBindingService{}
	platform.WithFeishuBindings(bindings)
	if platform.FeishuBindings != bindings {
		t.Fatal("feishu binding service was not attached")
	}
	for _, test := range []struct {
		name string
		call func() (*PlatformService, error)
	}{
		{name: "providers", call: func() (*PlatformService, error) {
			return NewPlatformService(nil, models, policies, skills, mcps, agents, sessions, runs, idempotency)
		}},
		{name: "models", call: func() (*PlatformService, error) {
			return NewPlatformService(providers, nil, policies, skills, mcps, agents, sessions, runs, idempotency)
		}},
		{name: "policies", call: func() (*PlatformService, error) {
			return NewPlatformService(providers, models, nil, skills, mcps, agents, sessions, runs, idempotency)
		}},
		{name: "skills", call: func() (*PlatformService, error) {
			return NewPlatformService(providers, models, policies, nil, mcps, agents, sessions, runs, idempotency)
		}},
		{name: "mcps", call: func() (*PlatformService, error) {
			return NewPlatformService(providers, models, policies, skills, nil, agents, sessions, runs, idempotency)
		}},
		{name: "agents", call: func() (*PlatformService, error) {
			return NewPlatformService(providers, models, policies, skills, mcps, nil, sessions, runs, idempotency)
		}},
		{name: "sessions", call: func() (*PlatformService, error) {
			return NewPlatformService(providers, models, policies, skills, mcps, agents, nil, runs, idempotency)
		}},
		{name: "runs", call: func() (*PlatformService, error) {
			return NewPlatformService(providers, models, policies, skills, mcps, agents, sessions, nil, idempotency)
		}},
		{name: "idempotency", call: func() (*PlatformService, error) {
			return NewPlatformService(providers, models, policies, skills, mcps, agents, sessions, runs, nil)
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := test.call(); err == nil {
				t.Fatal("missing dependency was accepted")
			}
		})
	}
}
