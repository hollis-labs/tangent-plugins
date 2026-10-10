package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	sdkmanifest "github.com/hollis-labs/libs/plugin-mcp/plugin-sdk/manifest"
	"github.com/hollis-labs/libs/plugin-mcp/plugin-sdk/subprocess"
	"github.com/hollis-labs/tangent/pkg/plugin"
)

func TestLifecycleKeepsReferencesPrivateAndNeverCreatesData(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	config := map[string]string{"backend": "nanite", "conversation_ref": "private-conversation-ref", "agent_ref": "private-agent-ref", "density": "compact", "rail_open": "true"}
	s := &served{}
	if _, err := s.Load(ctx); err == nil {
		t.Fatal("load before Init accepted")
	}
	if _, err := s.Init(ctx, subprocess.InitParams{Config: config, DataDir: dir}); err != nil {
		t.Fatal(err)
	}
	config["density"] = "comfortable"
	if _, err := s.Init(ctx, subprocess.InitParams{}); err == nil {
		t.Fatal("duplicate Init accepted")
	}
	if _, err := s.Load(ctx); err != nil {
		t.Fatal(err)
	}
	health, err := s.Health(ctx)
	if err != nil || !health.OK || !strings.Contains(health.Message, "backend_unavailable") {
		t.Fatal(health, err)
	}
	result, err := s.MCPCallTool(ctx, subprocess.MCPCallRequest{ToolName: statusTool})
	if err != nil || result.IsError {
		t.Fatal(result, err)
	}
	var status availability
	if err = json.Unmarshal(result.Content, &status); err != nil {
		t.Fatal(err)
	}
	if status.ChatAvailable || status.AgentRunning || !status.ConversationConfigured || !status.AgentConfigured || status.Density != "compact" || !status.RailOpen || len(status.Unavailable) == 0 {
		t.Fatal(status)
	}
	if bytes.Contains(result.Content, []byte("private-")) {
		t.Fatal("unbound caller received reference")
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 0 {
		t.Fatal("plugin persisted data", entries, err)
	}
	response, err := s.HTTPHandle(ctx, subprocess.HTTPRequest{Method: http.MethodGet, Path: statusPath})
	if err != nil || response.Status != 200 || !bytes.Equal(response.Body, result.Content) {
		t.Fatal(response, err)
	}
	if err = s.Unload(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err = s.MCPCallTool(ctx, subprocess.MCPCallRequest{ToolName: statusTool}); err == nil {
		t.Fatal("dispatch after Unload accepted")
	}
	response, err = s.HTTPHandle(ctx, subprocess.HTTPRequest{Method: http.MethodGet, Path: statusPath})
	if err != nil || response.Status != 503 {
		t.Fatal(response, err)
	}
	if s.config != (settings{}) {
		t.Fatal("unload retained reference")
	}
}

func TestRejectsContentCredentialsAuthorityAndInvalidPreferences(t *testing.T) {
	for _, config := range []map[string]string{
		{"history": "text"}, {"draft": "text"}, {"backend_token": "synthetic-secret"}, {"enabled": "true"}, {"participant_id": "claimed"},
		{"conversation_ref": "transcript with spaces"}, {"agent_ref": strings.Repeat("x", 129)}, {"backend": "tether"}, {"rail_open": "1"}, {"density": "invalid"},
	} {
		s := &served{}
		if _, err := s.Init(context.Background(), subprocess.InitParams{Config: config}); err == nil {
			t.Fatal("accepted invalid configuration", config)
		} else if strings.Contains(err.Error(), "synthetic-secret") {
			t.Fatal("diagnostic disclosed input")
		}
	}
	// No ambient config/credentials are consulted, even with entirely empty Config.
	t.Setenv("NANITE_AUTH_TOKEN", "synthetic-secret")
	t.Setenv("TANGENT_CHAT_AGENT_BACKEND", "tether")
	s := &served{}
	if _, err := s.Init(context.Background(), subprocess.InitParams{}); err != nil {
		t.Fatal(err)
	}
	if s.config.backend != "nanite" {
		t.Fatal("ambient fallback")
	}
	if _, err := s.Load(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, request := range []subprocess.MCPCallRequest{{ToolName: "tangent.chat_agent_send"}, {ToolName: statusTool, Arguments: map[string]any{"message": "draft"}}, {ToolName: statusTool, Arguments: map[string]any{"participant_id": "claim"}}} {
		if _, err := s.MCPCallTool(context.Background(), request); err == nil {
			t.Fatal("unsupported operation accepted")
		}
	}
	for _, request := range []subprocess.HTTPRequest{{Method: "POST", Path: statusPath}, {Method: "GET", Path: statusPath, Query: map[string]string{"session_id": "claim"}}, {Method: "GET", Path: statusPath, Body: []byte(`{}`)}} {
		result, err := s.HTTPHandle(context.Background(), request)
		if err != nil || result.Status < 400 {
			t.Fatal(result, err)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := s.MCPCallTool(ctx, subprocess.MCPCallRequest{ToolName: statusTool}); err == nil {
		t.Fatal("canceled dispatch accepted")
	}
}

func TestManifestVerifiesPayloadAndRefusesTampering(t *testing.T) {
	var out bytes.Buffer
	if err := run([]string{"--manifest"}, &out); err != nil {
		t.Fatal(err)
	}
	declaration, err := plugin.DecodeManifest(&out)
	if err != nil {
		t.Fatal(err)
	}
	if err = declaration.CheckCompatibility(sdkmanifest.Compatibility{Hosts: map[string]string{"tangent": "1.0.0"}, Engines: map[string]string{"binary": "1.0.0"}}); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	entry := filepath.Join(dir, filepath.FromSlash(declaration.Server.Entry))
	if err = os.MkdirAll(filepath.Dir(entry), 0700); err != nil {
		t.Fatal(err)
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	payload, err := os.ReadFile(executable)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(entry, payload, 0700); err != nil {
		t.Fatal(err)
	}
	var canonical bytes.Buffer
	if err = sdkmanifest.Encode(&canonical, declaration.Manifest); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(dir, sdkmanifest.Filename), canonical.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
	if err = declaration.VerifyBundle(dir); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(entry, []byte("modified"), 0700); err != nil {
		t.Fatal(err)
	}
	if err = declaration.VerifyBundle(dir); err == nil {
		t.Fatal("tampered payload accepted")
	}
	if len(declaration.Capabilities) != 0 || declaration.UI != nil || len(declaration.Config.Secrets) != 0 {
		t.Fatal("scaffold requested authority or credentials/UI")
	}
	if err = run([]string{"--manifest", "extra"}, &out); err == nil {
		t.Fatal("extra build arguments accepted")
	}
}

func TestConcurrentStatusAndUnloadFencesSnapshot(t *testing.T) {
	s := &served{}
	ctx := context.Background()
	if _, err := s.Init(ctx, subprocess.InitParams{Config: map[string]string{"conversation_ref": "private-ref"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Load(ctx); err != nil {
		t.Fatal(err)
	}
	start := make(chan struct{})
	var readers sync.WaitGroup
	for range 8 {
		readers.Go(func() {
			<-start
			for range 100 {
				raw, err := s.status(ctx)
				if err == nil && bytes.Contains(raw, []byte("private-ref")) {
					t.Error("reference leaked during teardown")
				}
				if err != nil && !errors.Is(err, errNotLoaded) {
					t.Error(err)
				}
				_, _ = s.Health(ctx)
			}
		})
	}
	close(start)
	if err := s.Unload(ctx); err != nil {
		t.Fatal(err)
	}
	readers.Wait()
	if _, err := s.status(ctx); !errors.Is(err, errNotLoaded) {
		t.Fatal("unload did not fence status", err)
	}
	if s.config != (settings{}) {
		t.Fatal("unload retained snapshot")
	}
}
