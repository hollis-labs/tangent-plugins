package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/hollis-labs/libs/plugin-mcp/plugin-sdk/subprocess"
)

func TestHostConfigurationPrecedenceAndRefusal(t *testing.T) {
	t.Setenv(configEnv, filepath.Join(t.TempDir(), "ambient.json"))
	t.Setenv("TETHER_TOKEN", "ambient-fixture-token")
	path := filepath.Join(t.TempDir(), "host.json")
	gotPath, token, err := configurationInputs(map[string]string{"configuration_path": path, "tether_token": "host-fixture-token"})
	if err != nil || gotPath != path || token != "host-fixture-token" {
		t.Fatal("host inputs not selected", err)
	}
	_, token, err = configurationInputs(map[string]string{"configuration_path": path})
	if err != nil || token != "" {
		t.Fatal("omitted host token acquired ambient authority", err)
	}
	for _, config := range []map[string]string{
		{"tether_token": "fixture-token"}, {"configuration_path": "relative.json"},
		{"configuration_path": ""}, {"configuration_path": path, "channels": "other"},
	} {
		if _, _, err := configurationInputs(config); err == nil {
			t.Fatal("invalid host input accepted")
		}
	}
}

func TestEmptyHostConfigurationRetainsStandaloneMode(t *testing.T) {
	path := filepath.Join(t.TempDir(), "standalone.json")
	t.Setenv(configEnv, path)
	t.Setenv("TETHER_TOKEN", "standalone-fixture-token")
	for _, config := range []map[string]string{nil, {}} {
		gotPath, token, err := configurationInputs(config)
		if err != nil || gotPath != path || token != "standalone-fixture-token" {
			t.Fatal("standalone inputs not selected", err)
		}
	}
}

func TestPartialHostConfigurationRefusesBeforeLedger(t *testing.T) {
	s := &served{}
	_, err := s.Init(context.Background(), subprocess.InitParams{DataDir: t.TempDir(), Config: map[string]string{"tether_token": "fixture-token"}})
	if err == nil || s.ledger != nil {
		t.Fatal("partial input initialized plugin")
	}
}

func settingsFixture(address string) map[string]string {
	return map[string]string{"configuration_mode": "settings", "endpoint_ref": "synthetic-tether", "tether_address": address, "caller_urn": "msg://service/local/messaging-fixture", "channels_json": `["owner-inbox"]`, "stages_json": `[{"id":"summarize","endpoint_url":"` + address + `","caller_id":"messaging-fixture","timeout_ms":2000}]`, "request_timeout_ms": "2000", "reconnect_min_ms": "10", "reconnect_max_ms": "100", "history_limit": "10"}
}
func TestHostSettingsResolveBeforeLedgerAndNeverFallBack(t *testing.T) {
	t.Setenv(configEnv, "ambient.json")
	t.Setenv("TETHER_TOKEN", "ambient-fixture-token")
	settings := settingsFixture("http://127.0.0.1:1")
	config, token, err := resolveConfiguration(settings)
	if err != nil || token != "" || config.Channels[0] != "owner-inbox" || config.Stages[0].ID != "summarize" {
		t.Fatal("settings not selected", err)
	}
	for name, change := range map[string]func(map[string]string){
		"missing_source":       func(m map[string]string) { delete(m, "endpoint_ref") },
		"partial":              func(m map[string]string) { delete(m, "history_limit") },
		"mixed_mode":           func(m map[string]string) { m["configuration_path"] = "/explicit.json" },
		"unknown_stage":        func(m map[string]string) { m["stages_json"] = `[{"id":"other"}]` },
		"duplicate_stage_key":  func(m map[string]string) { m["stages_json"] = `[{"id":"summarize","id":"summarize"}]` },
		"duplicate_channel":    func(m map[string]string) { m["channels_json"] = `["owner-inbox","owner-inbox"]` },
		"relative_instruction": func(m map[string]string) { m["instruction_path"] = "relative.md" },
		"conflicting_instruction": func(m map[string]string) {
			m["instruction_path"] = "/other.md"
			m["stages_json"] = `[{"id":"summarize","endpoint_url":"http://127.0.0.1:1","caller_id":"fixture","instruction_path":"/first.md"}]`
		},
	} {
		t.Run(name, func(t *testing.T) {
			m := settingsFixture("http://127.0.0.1:1")
			change(m)
			s := &served{}
			if _, err := s.Init(context.Background(), subprocess.InitParams{DataDir: t.TempDir(), Config: m}); err == nil || s.ledger != nil {
				t.Fatal("invalid settings acquired ledger")
			}
		})
	}
}

func TestBothHostModesInitializeFromProvidedInputs(t *testing.T) {
	for _, mode := range []string{"file", "settings"} {
		t.Run(mode, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { t.Error("Init called service"); w.WriteHeader(500) }))
			defer server.Close()
			path, data := fixtureConfiguration(t, server.URL)
			t.Setenv(configEnv, "invalid-ambient")
			t.Setenv("TETHER_TOKEN", "ambient-fixture-token")
			t.Setenv("TANGENT_MCP_URL", server.URL)
			inputs := settingsFixture(server.URL)
			if mode == "file" {
				inputs = map[string]string{"configuration_mode": "file", "configuration_path": path}
			}
			s := &served{}
			if _, err := s.Init(context.Background(), subprocess.InitParams{DataDir: data, Config: inputs}); err != nil {
				t.Fatal(err)
			}
			defer func() {
				if err := s.Unload(context.Background()); err != nil {
					t.Error(err)
				}
			}()
			inputs["endpoint_ref"] = "caller-mutated"
			if s.config.EndpointRef != "synthetic-tether" {
				t.Fatal("provided configuration not snapshotted")
			}
		})
	}
}
