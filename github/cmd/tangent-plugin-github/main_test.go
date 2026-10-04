package main

import (
	"bytes"
	"testing"

	sdkmanifest "github.com/hollis-labs/plugin-sdk/manifest"
	"github.com/hollis-labs/tangent-plugins/github/internal/github"
)

func TestManifestDeclaresProtectedRoutes(t *testing.T) {
	var out bytes.Buffer
	if err := emitManifest(&out); err != nil {
		t.Fatal(err)
	}
	declaration, err := sdkmanifest.Decode(&out)
	if err != nil {
		t.Fatal(err)
	}
	var manifest tangentExtension
	if err := sdkmanifest.DecodeExtension(declaration.Tangent, &manifest); err != nil {
		t.Fatal(err)
	}
	if len(declaration.Tools) != 1 || declaration.Tools[0].Name != github.OpenTool {
		t.Fatal("missing PR tool")
	}
	capabilities := map[string]string{}
	for _, route := range manifest.Routes {
		capabilities[route.Path] = route.Capability
	}
	if capabilities[github.StatePath] != "view" || capabilities[github.ActionPath] != "resolve" {
		t.Fatalf("unsafe routes: %#v", capabilities)
	}
}
