package mcpapi

import (
	"encoding/json"
	"testing"
)

func TestMCPScopedReadsStrictSharedSchemaAndNoLabelAuthority(t *testing.T) {
	s := shadow(t)
	a := New(s, verifier)
	in := map[string]any{"db": "ideas", "scope": map[string]any{"kind": "unscoped"}, "page": map[string]any{"limit": 1}}
	result, out := invoke(t, a, "list", "synthetic-reader", in)
	if result.IsError {
		t.Fatal(out)
	}
	envelope := out.(map[string]any)
	if envelope["total"] != json.Number("1") || len(envelope["membership"].(map[string]any)) != 1 {
		t.Fatal(out)
	}
	for _, scope := range []any{nil, "unscoped", map[string]any{"kind": "unscoped", "id": "invented"}} {
		result, out = invoke(t, a, "list", "synthetic-reader", map[string]any{"db": "ideas", "scope": scope})
		if !result.IsError || out.(map[string]any)["error"].(map[string]any)["code"] != "bad_request" {
			t.Fatal(out)
		}
	}
	result, _ = invoke(t, a, "list", "Architect", in)
	if !result.IsError {
		t.Fatal("scope became label authority")
	}
	result, _ = invoke(t, New(s, nil), "list", "synthetic-reader", in)
	if !result.IsError {
		t.Fatal("scope enabled default adapter")
	}
	// Discovery comes directly from the shared domain schema, not a transport copy.
	for _, tool := range Tools() {
		if tool.Name != Prefix+"list" {
			continue
		}
		var schema map[string]any
		if err := json.Unmarshal(tool.InputSchema, &schema); err != nil {
			t.Fatal(err)
		}
		if schema["properties"].(map[string]any)["scope"] == nil {
			t.Fatal("scope absent from advertised schema")
		}
	}
}
