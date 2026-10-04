package main

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/hollis-labs/plugin-sdk/capability"
	sdkmanifest "github.com/hollis-labs/plugin-sdk/manifest"
	"github.com/hollis-labs/plugin-sdk/subprocess"
)

func TestManifestVerifiesStagedPayload(t *testing.T) {
	var out bytes.Buffer
	if err := emitManifest(&out); err != nil {
		t.Fatal(err)
	}
	declaration, decodeErr := sdkmanifest.Decode(&out)
	if decodeErr != nil {
		t.Fatal(decodeErr)
	}
	var extension tangentExtension
	if err := sdkmanifest.DecodeExtension(declaration.Tangent, &extension); err != nil {
		t.Fatal(err)
	}
	for _, name := range extension.MCPTools {
		found := false
		for _, tool := range declaration.Tools {
			if tool.Name == name {
				found = true
			}
		}
		if !found {
			t.Fatalf("binding %s has no common tool declaration", name)
		}
	}
	if err := declaration.CheckCompatibility(sdkmanifest.Compatibility{Hosts: map[string]string{"tangent": "1.0.0"}, Engines: map[string]string{"binary": "1.0.0"}}); err != nil {
		t.Fatal(err)
	}
	if err := declaration.CheckCompatibility(sdkmanifest.Compatibility{Hosts: map[string]string{"tangent": "2.0.0"}, Engines: map[string]string{"binary": "1.0.0"}}); err == nil {
		t.Fatal("accepted unsupported host contract")
	}
	dir := t.TempDir()
	entry := filepath.Join(dir, filepath.FromSlash(declaration.Server.Entry))
	if err := os.MkdirAll(filepath.Dir(entry), 0700); err != nil {
		t.Fatal(err)
	}
	executable, executableErr := os.Executable()
	if executableErr != nil {
		t.Fatal(executableErr)
	}
	payload, readErr := os.ReadFile(executable)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if err := os.WriteFile(entry, payload, 0700); err != nil {
		t.Fatal(err)
	}
	var canonical bytes.Buffer
	if err := sdkmanifest.Encode(&canonical, declaration); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, sdkmanifest.Filename), canonical.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
	if err := declaration.VerifyBundle(dir); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(entry, []byte("changed payload"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := declaration.VerifyBundle(dir); err == nil {
		t.Fatal("accepted changed payload")
	}
}

func TestInitMatchesReviewedIdentity(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusServiceUnavailable) }))
	defer server.Close()
	t.Setenv("TANGENT_MCP_URL", server.URL)
	var out bytes.Buffer
	if err := emitManifest(&out); err != nil {
		t.Fatal(err)
	}
	declaration, decodeErr := sdkmanifest.Decode(&out)
	if decodeErr != nil {
		t.Fatal(decodeErr)
	}
	s := &served{}
	result, err := s.Init(context.Background(), subprocess.InitParams{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := s.Unload(context.Background()); err != nil {
			t.Error(err)
		}
	}()
	if result.ID != declaration.ID || result.Version != declaration.Version || result.Protocol != declaration.Protocol {
		t.Fatalf("Init identity disagrees with reviewed manifest: %#v", result)
	}
	if result.Protocol != 2 || result.CapabilityContract != capability.ContractVersion {
		t.Fatalf("missing protocol-2 capability acknowledgment: %#v", result)
	}
}
