package messaging

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	tether "github.com/hollis-labs/go-tether-client"
	pipeline "github.com/hollis-labs/libs/message-pipeline"
)

func TestBuiltinStageWiringUsesBaseEndpointAndExplicitCredentials(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/ai/chat" || r.Header.Get("Authorization") != "Bearer synthetic-token" {
			t.Error("owner endpoint/credential wiring changed")
		}
		var input tether.ChatRequest
		if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
			t.Error(err)
		}
		if input.Request.CallerID != "messaging-test" || input.Request.SessionID != "" || len(input.Request.Tools) != 0 {
			t.Error("stateless caller contract changed")
		}
		_ = json.NewEncoder(w).Encode(tether.ChatResponse{Response: tether.AIResponse{Output: []tether.AIMessage{{Role: "assistant", Parts: []tether.AIContentPart{{Type: "text", Text: `{"summary":"Fixture summary."}`}}}}, StopReason: "completed"}})
	}))
	defer server.Close()
	config := testConfig()
	config.Stages[0].EndpointURL = server.URL
	specs, closeStages, err := BuildStages(config, "synthetic-token")
	if err != nil {
		t.Fatal(err)
	}
	defer closeStages()
	result, err := specs[0].Stage.Run(context.Background(), pipeline.Input{Message: pipeline.Message{Identity: pipeline.Identity{Source: "source", Message: "message"}, Original: "owned fixture"}})
	if err != nil || len(result.Summaries) != 1 || result.Summaries[0].Text != "Fixture summary." {
		t.Fatal("built stage failed", result, err)
	}
}
