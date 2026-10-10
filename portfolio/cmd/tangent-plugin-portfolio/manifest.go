package main

import (
	"io"
	"os"

	sdkmanifest "github.com/hollis-labs/libs/plugin-mcp/plugin-sdk/manifest"
	"github.com/hollis-labs/tangent-plugins/portfolio/internal/httpapi"
	"github.com/hollis-labs/tangent-plugins/portfolio/internal/mcpapi"
	"github.com/hollis-labs/tangent/pkg/plugin"
)

func emitManifest(out io.Writer) error {
	executable, err := os.Executable()
	if err != nil {
		return err
	}
	file, err := os.Open(executable)
	if err != nil {
		return err
	}
	payload, readErr := io.ReadAll(file)
	closeErr := file.Close()
	if readErr != nil {
		return readErr
	}
	if closeErr != nil {
		return closeErr
	}
	bindings := plugin.TangentExtension{SchemaVersion: plugin.TangentSchemaVersion}
	for _, route := range httpapi.Routes() {
		bindings.Routes = append(bindings.Routes, route.Declaration)
	}
	tools := mcpapi.Tools()
	for _, tool := range tools {
		bindings.MCPTools = append(bindings.MCPTools, tool.Name)
	}
	// No UI, secrets, callbacks or administrative migrate exposure.
	return plugin.EncodeNativeManifest(out, payload, "bin/tangent-plugin-portfolio", sdkmanifest.Manifest{
		ID: pluginID, Name: pluginName, Version: pluginVersion, Description: pluginDescription, Tools: tools,
	}, bindings)
}
