package main

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"

	sdkmanifest "github.com/hollis-labs/libs/plugin-mcp/plugin-sdk/manifest"
)

// tangentExtension contains host bindings, never tool schemas or grants.
// Kind references resolve through the host's approved definition catalog.
type tangentExtension struct {
	SchemaVersion int         `json:"schema_version"`
	Kinds         []kindRef   `json:"kinds,omitempty"`
	Routes        []routeDecl `json:"routes,omitempty"`
	MCPTools      []string    `json:"mcp_tools,omitempty"`
}
type kindRef struct {
	Kind    string `json:"kind"`
	Version string `json:"version"`
	Package string `json:"package"`
}
type routeDecl struct {
	Method     string `json:"method"`
	Path       string `json:"path"`
	Capability string `json:"capability"`
}

// executableArtifact hashes the already-built native payload. It does not
// inspect configuration or start the plugin lifecycle. The build stages this
// same executable under entry, then captures the declaration beside bin/.
func executableArtifact(entry string) (sdkmanifest.Artifact, error) {
	executable, err := os.Executable()
	if err != nil {
		return sdkmanifest.Artifact{}, err
	}
	f, err := os.Open(executable)
	if err != nil {
		return sdkmanifest.Artifact{}, err
	}
	h := sha256.New()
	_, readErr := io.Copy(h, f)
	closeErr := f.Close()
	if readErr != nil {
		return sdkmanifest.Artifact{}, readErr
	}
	if closeErr != nil {
		return sdkmanifest.Artifact{}, closeErr
	}
	files := []sdkmanifest.ArtifactFile{{Path: entry, SHA256: hex.EncodeToString(h.Sum(nil)), Executable: true}}
	digest, err := sdkmanifest.TreeDigest(files)
	if err != nil {
		return sdkmanifest.Artifact{}, err
	}
	return sdkmanifest.Artifact{Files: files, TreeSHA256: digest}, nil
}
