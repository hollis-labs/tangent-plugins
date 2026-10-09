package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/hollis-labs/libs/plugin-mcp/plugin-sdk/capability"
	sdkmanifest "github.com/hollis-labs/libs/plugin-mcp/plugin-sdk/manifest"
	"github.com/hollis-labs/libs/plugin-mcp/plugin-sdk/subprocess"
	"github.com/hollis-labs/tangent-plugins/messaging/internal/messaging"
)

func TestManifestVerifiesStagedPayload(t *testing.T) {
	var out bytes.Buffer
	if err := emitManifest(&out); err != nil {
		t.Fatal(err)
	}
	declaration, err := sdkmanifest.Decode(&out)
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
	if err = sdkmanifest.Encode(&canonical, declaration); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(dir, sdkmanifest.Filename), canonical.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
	if err = declaration.VerifyBundle(dir); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(entry, []byte("changed payload"), 0700); err != nil {
		t.Fatal(err)
	}
	if err = declaration.VerifyBundle(dir); err == nil {
		t.Fatal("accepted changed native payload")
	}
}

func fixtureConfiguration(t *testing.T, address string) (string, string) {
	t.Helper()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	config := messaging.Config{SchemaVersion: 1, EndpointRef: "synthetic-tether", TetherAddress: address, CallerURN: "msg://service/local/messaging-fixture", Channels: []string{"owner-inbox"}, RequestTimeoutMS: 2000, ReconnectMinMS: 10, ReconnectMaxMS: 100, HistoryLimit: 10, Stages: []messaging.StageConfig{{ID: "summarize", EndpointURL: address, CallerID: "messaging-fixture", TimeoutMS: 2000}}}
	raw, err := json.Marshal(config)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "configuration.json")
	if err = os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	return path, dir
}

func TestInitSnapshotsConfigurationWithoutCallingServices(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer server.Close()
	path, dir := fixtureConfiguration(t, server.URL)
	t.Setenv(configEnv, path)
	t.Setenv("TANGENT_MCP_URL", server.URL)
	t.Setenv("TETHER_TOKEN", "")
	s := &served{}
	result, err := s.Init(context.Background(), subprocess.InitParams{DataDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := s.Unload(context.Background()); err != nil {
			t.Error(err)
		}
	})
	if result.ID != pluginID || result.Version != pluginVersion || result.Protocol != 2 || result.CapabilityContract != capability.ContractVersion || calls.Load() != 0 {
		t.Fatal("Init identity or lazy ports disagreed", result, calls.Load())
	}
	// Later edits cannot substitute an already initialized stage/configuration.
	if err = os.WriteFile(path, []byte(`{}`), 0600); err != nil {
		t.Fatal(err)
	}
	if s.config.EndpointRef != "synthetic-tether" || s.specs[0].ConfigDigest == "" {
		t.Fatal("Init retained mutable configuration")
	}
	if _, err = s.Init(context.Background(), subprocess.InitParams{DataDir: dir}); err == nil {
		t.Fatal("duplicate Init accepted")
	}
}

func TestInitRefusalsReleasePrivateLedgerAndNeverDial(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer server.Close()
	path, dir := fixtureConfiguration(t, server.URL)
	t.Setenv(configEnv, path)
	t.Setenv("TANGENT_MCP_URL", "")
	s := &served{}
	if _, err := s.Init(context.Background(), subprocess.InitParams{DataDir: dir}); err == nil {
		t.Fatal("missing host accepted")
	}
	ledger, err := messaging.OpenLedger(context.Background(), dir)
	if err != nil {
		t.Fatal("failed Init retained lock", err)
	}
	if err = ledger.Close(); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TANGENT_MCP_URL", server.URL)
	if _, err = s.Init(context.Background(), subprocess.InitParams{DataDir: "relative"}); err == nil {
		t.Fatal("ambient DataDir accepted")
	}
	t.Setenv(configEnv, "relative")
	if _, err = s.Init(context.Background(), subprocess.InitParams{DataDir: dir}); err == nil {
		t.Fatal("ambient configuration accepted")
	}
	if calls.Load() != 0 {
		t.Fatal("refused Init called service")
	}
}
