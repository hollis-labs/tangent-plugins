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
		Config:       sdkmanifest.Config{Fields: map[string]sdkmanifest.Field{"configuration_path": {Type: "string", Label: "Explicit non-secret messaging JSON configuration", Required: true, Env: configEnv}}, Secrets: map[string]sdkmanifest.Secret{"tether_token": {Label: "Explicit Tether token if required by the configured endpoints", Env: "TETHER_TOKEN"}}},
		Tangent:      extension,
	})
}
