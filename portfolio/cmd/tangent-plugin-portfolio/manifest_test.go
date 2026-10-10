package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hollis-labs/libs/plugin-mcp/plugin-sdk/capability"
	sdkmanifest "github.com/hollis-labs/libs/plugin-mcp/plugin-sdk/manifest"
	"github.com/hollis-labs/libs/plugin-mcp/plugin-sdk/subprocess"
	"github.com/hollis-labs/tangent-plugins/portfolio/internal/httpapi"
	"github.com/hollis-labs/tangent/pkg/plugin"
)

func TestNativeManifestPayloadAndRouteContracts(t *testing.T) {
	var out bytes.Buffer
	if err := run([]string{"--manifest"}, &out); err != nil {
		t.Fatal(err)
	}
	manifestRaw := bytes.Clone(out.Bytes())
	m, err := plugin.DecodeManifest(&out)
	if err != nil {
		t.Fatal(err)
	}
	if m.ID != pluginID || m.UI != nil || len(m.Hooks) != 0 || len(m.Capabilities) != 0 {
		t.Fatal("unexpected surface", m)
	}
	for _, tool := range m.Tools {
		if tool.Name == "tangent.portfolio_migrate" {
			t.Fatal("administrative MCP exposure")
		}
	}
	if err = m.CheckCompatibility(sdkmanifest.Compatibility{Hosts: map[string]string{"tangent": "1.0.0"}, Engines: map[string]string{"binary": "1.0.0"}}); err != nil {
		t.Fatal(err)
	}
	// Pin representative semantic route choices, not registry counts or
	// mutable-file equality. The common decoder checks literal owner paths.
	seen := map[string]string{}
	for _, route := range m.Bindings.Routes {
		if route.Method != "POST" || !strings.HasPrefix(route.Path, httpapi.Prefix) {
			t.Fatal(route)
		}
		seen[route.Path] = route.Capability
	}
	for path, want := range map[string]string{httpapi.Prefix + "get": "view", httpapi.Prefix + "update": "draft", httpapi.Prefix + "decide": "resolve", httpapi.Prefix + "defer": "resolve", httpapi.Prefix + "reopen": "resolve"} {
		if seen[path] != want {
			t.Fatal(path, seen[path], want)
		}
	}
	if _, exists := seen[httpapi.Prefix+"migrate"]; exists {
		t.Fatal("administrative migrate exposed")
	}
	dir := t.TempDir()
	if err = os.WriteFile(filepath.Join(dir, "plugin.yaml"), manifestRaw, 0600); err != nil {
		t.Fatal(err)
	}
	entry := filepath.Join(dir, filepath.FromSlash(m.Server.Entry))
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
	if err = m.VerifyBundle(dir); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(entry, []byte("tampered"), 0700); err != nil {
		t.Fatal(err)
	}
	if err = m.VerifyBundle(dir); err == nil {
		t.Fatal("tampered native payload accepted")
	}
}

func TestDefaultLifecycleNeverOpensDataOrAdmitsCallers(t *testing.T) {
	dir := t.TempDir()
	// An explicit persistent DataDir is host-owned metadata, not permission
	// for this source-only executable to create a store or discover a tracker.
	s := &served{}
	if _, err := s.Load(t.Context()); err == nil {
		t.Fatal("load before init")
	}
	if _, err := s.Init(t.Context(), subprocess.InitParams{DataDir: dir, Identity: json.RawMessage(`{"principal":"owner","verified":true}`)}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Init(t.Context(), subprocess.InitParams{}); err == nil {
		t.Fatal("duplicate init")
	}
	if _, err := s.Load(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Load(t.Context()); err == nil {
		t.Fatal("duplicate load")
	}
	for _, route := range httpapi.Routes() {
		for _, identity := range []json.RawMessage{nil, json.RawMessage(`{"principal":"Architect","verified":true,"allowed":true}`)} {
			got, err := s.HTTPHandle(t.Context(), subprocess.HTTPRequest{Method: route.Declaration.Method, Path: route.Declaration.Path, Headers: map[string]string{"Content-Type": "application/json", "X-Client": "browser"}, Identity: identity, Body: []byte(`{"author":"owner"}`)})
			if err != nil || got.Status != 503 || !bytes.Contains(got.Body, []byte("authority unavailable")) {
				t.Fatal(got, err)
			}
		}
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 0 {
		t.Fatal("default executable wrote data", entries, err)
	}
	for _, name := range []string{"tangent.portfolio_get", "tangent.portfolio_comment", "tangent.portfolio_decide", "tangent.portfolio_batch"} {
		result, callErr := s.MCPCallTool(t.Context(), subprocess.MCPCallRequest{ToolName: name, Arguments: map[string]any{}, Identity: json.RawMessage(`{"principal":"owner","verified":true}`)})
		if callErr != nil || !result.IsError || !bytes.Contains(result.Content, []byte("verified caller authority unavailable")) {
			t.Fatal("default MCP admitted caller", result, callErr)
		}
	}
	if err = s.Unload(t.Context()); err != nil {
		t.Fatal(err)
	}
	got, err := s.HTTPHandle(t.Context(), subprocess.HTTPRequest{Method: "POST", Path: httpapi.Prefix + "get"})
	if err != nil || got.Status != 503 {
		t.Fatal(got, err)
	}
	if _, err = s.Init(t.Context(), subprocess.InitParams{Config: map[string]string{"data_root": "/live", "token": "synthetic-credential"}}); err == nil || strings.Contains(err.Error(), "synthetic-credential") {
		t.Fatal("configuration authority accepted or leaked", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err = s.Init(ctx, subprocess.InitParams{}); err == nil {
		t.Fatal("cancelled init accepted")
	}
}

func TestSDKWireDefaultRefusesAssertedIdentity(t *testing.T) {
	// Actual SDK protocol server with test-owned streams; no host origin guard,
	// deployed issuer, installed plugin or live writer is exercised.
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	in, input := io.Pipe()
	output, out := io.Pipe()
	defer in.Close()
	defer input.Close()
	defer output.Close()
	defer out.Close()
	done := make(chan error, 1)
	go func() {
		done <- subprocess.ServeWithOptions(&served{}, subprocess.ServeOptions{Context: ctx, Input: in, Output: out})
	}()
	encoder := json.NewEncoder(input)
	decoder := json.NewDecoder(output)
	call := func(id int64, method string, params any) subprocess.RPCResponse {
		t.Helper()
		if err := encoder.Encode(subprocess.RPCRequest{JSONRPC: "2.0", ID: subprocess.NumberID(id), Method: method, Params: params}); err != nil {
			t.Fatal(err)
		}
		var reply subprocess.RPCResponse
		if err := decoder.Decode(&reply); err != nil {
			t.Fatal(err)
		}
		if reply.Error != nil {
			t.Fatal(reply.Error)
		}
		return reply
	}
	dir := t.TempDir()
	call(1, subprocess.MethodInit, subprocess.InitParams{PluginDir: dir, DataDir: filepath.Join(dir, "data"), CacheDir: filepath.Join(dir, "cache"), Config: map[string]string{}, LogLevel: "info", HostInfo: subprocess.HostInfo{Version: "fixture", Protocol: subprocess.ProtocolVersion}, CapabilityContract: capability.ContractVersion, Incarnation: capability.RuntimeIdentity{HostInstance: "test-host", OwnerID: "test-plugin", OwnerGeneration: 1}})
	call(2, subprocess.MethodLoad, map[string]any{})
	reply := call(3, "http/handle", subprocess.HTTPRequest{Method: "POST", Path: httpapi.Prefix + "get", Headers: map[string]string{"Content-Type": "application/json"}, Body: []byte(`{"db":"ideas","id":"ID-one"}`), Identity: json.RawMessage(`{"principal":"owner","verified":true}`)})
	raw, err := json.Marshal(reply.Result)
	if err != nil {
		t.Fatal(err)
	}
	var got subprocess.HTTPResponse
	if err = json.Unmarshal(raw, &got); err != nil || got.Status != 503 {
		t.Fatal(got, err)
	}
	call(4, subprocess.MethodUnload, map[string]any{})
	input.Close()
	if err = <-done; err != nil {
		t.Fatal(err)
	}
}
