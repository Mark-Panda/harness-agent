//go:build e2e

package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"os"
	"testing"
	"time"

	harnessv1 "github.com/Mark-Panda/harness-agent/api/harness/v1"
	"github.com/google/uuid"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

// TestLiveGRPCContract 针对运行中的容器验证 gRPC 鉴权与用户身份透传。
// 运行方式：GRPC_E2E_ADDR=localhost:9190 HTTP_E2E_BASE=http://localhost:8080 go test -tags=e2e ./internal/server -run TestLiveGRPCContract
func TestLiveGRPCContract(t *testing.T) {
	grpcAddress := os.Getenv("GRPC_E2E_ADDR")
	httpBase := os.Getenv("HTTP_E2E_BASE")
	if grpcAddress == "" || httpBase == "" {
		t.Skip("GRPC_E2E_ADDR and HTTP_E2E_BASE are required")
	}
	username := "grpc_e2e_" + uuid.NewString()[:12]
	body, _ := json.Marshal(map[string]string{"username": username, "password": "CorrectHorseBatteryStaple123!"})
	response, err := http.Post(httpBase+"/api/v1/auth/register", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusCreated {
		t.Fatalf("register returned %d", response.StatusCode)
	}
	var registered struct {
		User struct {
			ID string `json:"id"`
		} `json:"user"`
		Tokens struct {
			AccessToken string `json:"access_token"`
		} `json:"tokens"`
	}
	if err := json.NewDecoder(response.Body).Decode(&registered); err != nil {
		t.Fatal(err)
	}

	connection, err := grpc.NewClient(grpcAddress, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	client := harnessv1.NewConversationServiceClient(connection)

	// 无 Token 调用必须被拦截。
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := client.CreateSession(ctx, &harnessv1.CreateSessionRequest{Title: "denied"}); status.Code(err) != codes.Unauthenticated {
		t.Fatalf("unauthenticated gRPC call error = %v", err)
	}

	authContext := metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer "+registered.Tokens.AccessToken)
	created, err := client.CreateSession(authContext, &harnessv1.CreateSessionRequest{Title: "grpc-e2e"})
	if err != nil {
		t.Fatal(err)
	}
	if created.GetSession().GetId() == "" || created.GetSession().GetStatus() != "active" {
		t.Fatalf("unexpected session response: %#v", created.GetSession())
	}

	// 通过 HTTP 读取同一会话，证明 gRPC 与 HTTP 共享同一用户隔离域。
	request, _ := http.NewRequest(http.MethodGet, httpBase+"/api/v1/sessions/"+created.GetSession().GetId(), nil)
	request.Header.Set("Authorization", "Bearer "+registered.Tokens.AccessToken)
	sessionResponse, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer sessionResponse.Body.Close()
	if sessionResponse.StatusCode != http.StatusOK {
		t.Fatalf("HTTP session lookup returned %d", sessionResponse.StatusCode)
	}
}
