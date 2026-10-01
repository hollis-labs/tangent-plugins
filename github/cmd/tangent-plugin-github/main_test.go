package main

import (
	"bytes"
	"testing"

	"github.com/hollis-labs/tangent-plugins/github/internal/github"
	tangentplugin "github.com/hollis-labs/tangent/pkg/plugin"
	"gopkg.in/yaml.v3"
)

func TestManifestDeclaresProtectedRoutes(t *testing.T) {
	var out bytes.Buffer
	if err := emitManifest(&out); err != nil {
		t.Fatal(err)
	}
	var manifest tangentplugin.Manifest
	if err := yaml.Unmarshal(out.Bytes(), &manifest); err != nil {
		t.Fatal(err)
	}
	if err := manifest.Validate(); err != nil {
		t.Fatal(err)
	}
	if len(manifest.Tools) != 1 || manifest.Tools[0].Name != github.OpenTool {
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
