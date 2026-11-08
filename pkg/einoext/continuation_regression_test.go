package einoext

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/Mark-Panda/harness-agent/pkg/agent/model"
	"github.com/Mark-Panda/harness-agent/pkg/agent/react"
)

func TestReactAgentRejectsContentFilteredContinuation(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		if calls.Add(1) == 1 {
			fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"partial\"},\"finish_reason\":\"length\"}]}\n\n")
		} else {
			fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"content_filter\"}]}\n\n")
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer server.Close()
	router, err := model.NewRouter([]model.Candidate{{
		Ref: model.ModelRef{
			ProviderID: "provider", ModelID: "model", RemoteModelName: "test-model", ContextWindowTokens: 1_000_000,
		},
		BaseURL: server.URL, APIKey: "test-key",
	}}, nil, model.RouterConfig{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	agent, err := NewReactAgent(context.Background(), ReactConfig{Router: router, Registry: testRegistry{}, MaxSteps: 4})
	if err != nil {
		t.Fatal(err)
	}
	result, err := agent.Run(context.Background(), []react.Message{{Role: react.RoleUser, Content: "Write a long answer."}})
	if !errors.Is(err, react.ErrContentFiltered) || result.Steps != 2 || calls.Load() != 2 {
		t.Fatalf("Run() = %#v, %v, with %d calls; want ErrContentFiltered after two calls", result, err, calls.Load())
	}
}
