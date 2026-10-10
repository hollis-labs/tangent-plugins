package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	pluginhost "github.com/hollis-labs/libs/plugin-mcp/plugin-host"
	"github.com/hollis-labs/libs/plugin-mcp/plugin-sdk/capability"
	"github.com/hollis-labs/libs/plugin-mcp/plugin-sdk/subprocess"
	"github.com/hollis-labs/tangent-plugins/messaging/internal/messaging"
)

// The public driver starts this exact compiled test executable as the real
// served plugin. No external service, provider, installed bundle or build runs.
func TestProtocolFixtureProcess(t *testing.T) {
	if os.Getenv("MESSAGING_PROTOCOL_FIXTURE") != "1" {
		return
	}
	if err := subprocess.Serve(&served{}); err != nil {
		t.Fatal(err)
	}
	os.Exit(0)
}

func TestProtocolLoadAndUnloadJoinSubscriptionAndReap(t *testing.T) {
	entered, joined := make(chan struct{}), make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer owned-host-fixture" {
			t.Error("runtime host token not used")
		}
		switch r.URL.Path {
		case "/channels/owner-inbox/messages":
			w.Header().Set("Content-Type", "application/json")
			if err := json.NewEncoder(w).Encode(map[string]any{"name": "owner-inbox", "address": "msg://service/local/channel/owner-inbox", "messages": []any{}, "next_since": 9999}); err != nil {
				t.Error(err)
			}
		case "/channels/owner-inbox/subscribe":
			if r.URL.Query().Get("since") != "0" {
				t.Error("absent cursor was not explicit zero")
			}
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			if _, err := fmt.Fprint(w, ": owned stream\n\n"); err != nil {
				t.Error(err)
			}
			w.(http.Flusher).Flush()
			close(entered)
			<-r.Context().Done()
			close(joined)
		default:
			t.Error("unexpected upstream operation", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()
	_, dataDir := fixtureConfiguration(t, server.URL)
	inputs := settingsFixture(server.URL)
	inputs["tether_token"] = "owned-host-fixture"
	cacheDir := t.TempDir()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	process, err := pluginhost.Start(ctx, pluginhost.Spec{ID: pluginID, ExpectedID: pluginID, ExpectedVersion: pluginVersion, Command: executable, Args: []string{"-test.run=^TestProtocolFixtureProcess$"}, Env: []string{"MESSAGING_PROTOCOL_FIXTURE=1", configEnv + "=invalid-ambient", "TANGENT_MCP_URL=" + server.URL}, Init: subprocess.InitParams{PluginDir: filepath.Dir(executable), DataDir: dataDir, CacheDir: cacheDir, Config: inputs, CapabilityContract: capability.ContractVersion, Incarnation: capability.RuntimeIdentity{HostInstance: "synthetic-messaging-host", OwnerID: pluginID, OwnerGeneration: 1}, Grants: capability.GrantSet{}, HostInfo: subprocess.HostInfo{Version: "1.0.0", Protocol: 2}}, HandshakeTimeout: 10 * time.Second, UnloadTimeout: 5 * time.Second, ReapTimeout: 2 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if stopErr := process.Stop(context.Background()); stopErr != nil {
			t.Error(stopErr)
		}
	})
	cancel() // The completed handshake must not own the loaded subscription.
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("actual subscription not reached", process.Diagnostics())
	}
	select {
	case <-joined:
		t.Fatal("Load completion cancelled subscription")
	default:
	}
	stop, end := context.WithTimeout(context.Background(), 5*time.Second)
	defer end()
	if err = process.Stop(stop); err != nil {
		t.Fatal(err)
	}
	select {
	case <-joined:
	case <-time.After(time.Second):
		t.Fatal("Unload did not retire actual HTTP reader")
	}
	select {
	case <-process.Exited():
	default:
		t.Fatal("child was not reaped")
	}
	info, ok := process.ExitInfo()
	if !ok || info.Code != 0 {
		t.Fatal("natural child exit failed", info, process.Diagnostics())
	}
	ledger, err := messaging.OpenLedger(context.Background(), dataDir)
	if err != nil {
		t.Fatal("Unload left ledger custody active", err)
	}
	defer ledger.Close()
	if _, found, err := ledger.Cursor(context.Background(), messaging.Source{EndpointRef: "synthetic-tether", Channel: "owner-inbox"}); err != nil || found {
		t.Fatal("history high water advanced cursor", err)
	}
}
