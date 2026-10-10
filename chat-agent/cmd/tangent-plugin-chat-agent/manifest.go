package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"

	sdkmanifest "github.com/hollis-labs/libs/plugin-mcp/plugin-sdk/manifest"
	"github.com/hollis-labs/libs/plugin-mcp/plugin-sdk/subprocess"
	"github.com/hollis-labs/tangent/pkg/plugin"
)

func executableArtifact(entry string) (sdkmanifest.Artifact, error) {
	executable, err := os.Executable()
	if err != nil {
		return sdkmanifest.Artifact{}, err
	}
	file, err := os.Open(executable)
	if err != nil {
		return sdkmanifest.Artifact{}, err
	}
	digest := sha256.New()
	_, readErr := io.Copy(digest, file)
	if err = errors.Join(readErr, file.Close()); err != nil {
		return sdkmanifest.Artifact{}, err
	}
	files := []sdkmanifest.ArtifactFile{{Path: entry, SHA256: hex.EncodeToString(digest.Sum(nil)), Executable: true}}
	tree, err := sdkmanifest.TreeDigest(files)
	if err != nil {
		return sdkmanifest.Artifact{}, err
	}
	return sdkmanifest.Artifact{Files: files, TreeSHA256: tree}, nil
}

func emitManifest(out io.Writer) error {
	entry := "bin/tangent-plugin-chat-agent"
	artifact, err := executableArtifact(entry)
	if err != nil {
		return err
	}
	extension, err := json.Marshal(plugin.TangentExtension{
		SchemaVersion: plugin.TangentSchemaVersion,
		Routes:        []plugin.RouteDecl{{Method: http.MethodGet, Path: statusPath, Capability: string(plugin.CapabilityView)}},
		MCPTools:      []string{statusTool},
	})
	if err != nil {
		return err
	}
	return sdkmanifest.Encode(out, sdkmanifest.Manifest{
		SchemaVersion: sdkmanifest.SchemaVersion, ID: pluginID, Name: pluginName, Version: pluginVersion, Description: pluginDescription,
		Runtime: sdkmanifest.Runtime, Protocol: subprocess.ProtocolVersion,
		Hosts:  map[string]sdkmanifest.HostRange{"tangent": {Min: "1.0.0", Max: "1.99.99"}},
		Server: sdkmanifest.Server{Runtime: "binary", Entry: entry, Engines: map[string]sdkmanifest.HostRange{"binary": {Min: "1.0.0", Max: "1.99.99"}}}, Artifact: artifact,
		Tools:   []sdkmanifest.Tool{{Name: statusTool, Description: "Read chat scaffold availability; never starts an agent or returns conversation content", InputSchema: json.RawMessage(`{"type":"object","additionalProperties":false,"properties":{}}`), Effect: "read", Annotations: &sdkmanifest.ToolAnnotations{ReadOnlyHint: boolPointer(true), IdempotentHint: boolPointer(true), DestructiveHint: boolPointer(false), OpenWorldHint: boolPointer(false)}}},
		Config:  sdkmanifest.Config{Fields: configurationFields()},
		Tangent: extension,
	})
}

// These host-managed scalars are references and presentation preferences only.
// No endpoint, credentials, instructions, transcript or enabled flag is accepted.
func configurationFields() map[string]sdkmanifest.Field {
	return map[string]sdkmanifest.Field{
		"backend":          {Type: "select", Label: "Backend (integration unavailable)", Default: "nanite", Options: []string{"nanite"}},
		"conversation_ref": {Type: "string", Label: "Opaque conversation reference", Description: "Reference only; does not create, resume or authorize a conversation"},
		"agent_ref":        {Type: "string", Label: "Opaque agent reference", Description: "Reference only; does not enroll or start an agent"},
		"rail_open":        {Type: "boolean", Label: "Preferred rail visibility (UI unavailable)", Default: "false"},
		"density":          {Type: "select", Label: "Preferred display density", Default: "comfortable", Options: []string{"comfortable", "compact"}},
	}
}

func boolPointer(value bool) *bool { return &value }
