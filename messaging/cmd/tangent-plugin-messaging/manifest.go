package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"

	"github.com/hollis-labs/libs/plugin-mcp/plugin-sdk/capability"
	sdkmanifest "github.com/hollis-labs/libs/plugin-mcp/plugin-sdk/manifest"
	"github.com/hollis-labs/libs/plugin-mcp/plugin-sdk/subprocess"
	"github.com/hollis-labs/tangent-plugins/messaging/internal/replyprojection"
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
	entry := "bin/tangent-plugin-messaging"
	artifact, err := executableArtifact(entry)
	if err != nil {
		return err
	}
	extension, err := json.Marshal(plugin.TangentExtension{SchemaVersion: plugin.TangentSchemaVersion, Routes: []plugin.RouteDecl{
		{Method: http.MethodGet, Path: replyprojection.DeliveryPath, Capability: string(plugin.CapabilityView)},
		{Method: http.MethodPost, Path: replyprojection.RetryPath, Capability: string(plugin.CapabilityDraft)},
	}})
	if err != nil {
		return err
	}
	return sdkmanifest.Encode(out, sdkmanifest.Manifest{
		SchemaVersion: sdkmanifest.SchemaVersion, ID: pluginID, Name: "Messaging Consumer", Version: pluginVersion, Description: "Durable publication intake with bounded stateless stage processing",
		Runtime: sdkmanifest.Runtime, Protocol: subprocess.ProtocolVersion,
		Hosts:  map[string]sdkmanifest.HostRange{"tangent": {Min: "1.0.0", Max: "1.99.99"}},
		Server: sdkmanifest.Server{Runtime: "binary", Entry: entry, Engines: map[string]sdkmanifest.HostRange{"binary": {Min: "1.0.0", Max: "1.99.99"}}}, Artifact: artifact,
		Capabilities: []subprocess.CapabilityRequest{{Name: capability.MCPReach, Reason: "Enqueue publications and await/acknowledge actual resolved replies through public Tangent tools", Metadata: json.RawMessage(`{"tools":["tangent.turns_enqueue","tangent.turn_await","tangent.turn_ack"]}`)}},
		Config:       sdkmanifest.Config{Fields: configurationFields(), Secrets: map[string]sdkmanifest.Secret{"tether_token": {Label: "Explicit Tether token if required by the configured endpoints", Env: "TETHER_TOKEN"}}},
		Tangent:      extension,
	})
}

// Fields stay flat; serialized list content is interpreted only by the plugin.
func configurationFields() map[string]sdkmanifest.Field {
	return map[string]sdkmanifest.Field{
		"configuration_mode": {Type: "select", Label: "Configuration mode", Required: true, Options: []string{"file", "settings"}},
		"configuration_path": {Type: "string", Label: "File mode: absolute non-secret JSON path", Env: configEnv},
		"endpoint_ref":       {Type: "string", Label: "Settings mode: source endpoint identity"},
		"tether_address":     {Type: "string", Label: "Settings mode: explicit Tether address"},
		"caller_urn":         {Type: "string", Label: "Settings mode: actual caller URN"},
		"channels_json":      {Type: "string", Label: "Settings mode: channels (JSON string array)"},
		"stages_json":        {Type: "string", Label: "Settings mode: stage list (JSON array)"},
		"instruction_path":   {Type: "string", Label: "Settings mode: absolute summarizer instruction document"},
		"request_timeout_ms": {Type: "integer", Label: "Settings mode: request timeout (ms)"},
		"reconnect_min_ms":   {Type: "integer", Label: "Settings mode: minimum reconnect delay (ms)"},
		"reconnect_max_ms":   {Type: "integer", Label: "Settings mode: maximum reconnect delay (ms)"},
		"history_limit":      {Type: "integer", Label: "Settings mode: history page limit"},
	}
}
