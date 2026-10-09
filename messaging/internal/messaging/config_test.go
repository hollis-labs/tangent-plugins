package messaging

import (
	"encoding/json"
	"strings"
	"testing"
)

func testConfig() Config {
	return Config{SchemaVersion: 1, EndpointRef: "test-tether", TetherAddress: "http://127.0.0.1:8998", CallerURN: "msg://service/local/owner-inbox", Channels: []string{"owner-inbox"}, RequestTimeoutMS: 15000, ReconnectMinMS: 100, ReconnectMaxMS: 5000, HistoryLimit: 100, Stages: []StageConfig{{ID: "summarize", EndpointURL: "http://fixture.test/ai/chat", CallerID: "messaging-test"}}}
}

func TestConfigExplicitInputsAndBounds(t *testing.T) {
	config := testConfig()
	raw, err := json.Marshal(config)
	if err != nil {
		t.Fatal(err)
	}
	got, err := ReadConfig(strings.NewReader(string(raw)))
	if err != nil || got.CallerURN != config.CallerURN || got.RequestTimeout() <= 0 {
		t.Fatalf("read: %+v %v", got, err)
	}
	for name, change := range map[string]func(*Config){
		"missing caller":            func(c *Config) { c.CallerURN = "" },
		"missing endpoint identity": func(c *Config) { c.EndpointRef = "" },
		"missing endpoint":          func(c *Config) { c.TetherAddress = "" },
		"embedded credentials":      func(c *Config) { c.TetherAddress = "https://user:secret@example.test" },
		"ambient socket":            func(c *Config) { c.TetherAddress = "unix:~/.tether/socket" },
		"no channels":               func(c *Config) { c.Channels = nil },
		"duplicate channels":        func(c *Config) { c.Channels = []string{"owner-inbox", "owner-inbox"} },
		"channel path":              func(c *Config) { c.Channels = []string{"../other"} },
		"unbounded request":         func(c *Config) { c.RequestTimeoutMS = 0 },
		"backoff reversed":          func(c *Config) { c.ReconnectMaxMS = c.ReconnectMinMS - 1 },
		"unbounded history":         func(c *Config) { c.HistoryLimit = 1001 },
		"no stages":                 func(c *Config) { c.Stages = nil },
		"unknown stage":             func(c *Config) { c.Stages[0].ID = "execute-agent" },
		"invalid ai endpoint":       func(c *Config) { c.Stages[0].EndpointURL = "ftp://fixture.test" },
		"unbounded ai":              func(c *Config) { c.Stages[0].TimeoutMS = 60001 },
		"ambient instructions":      func(c *Config) { c.Stages[0].InstructionPath = "~/.instruction" },
	} {
		t.Run(name, func(t *testing.T) {
			c := testConfig()
			change(&c)
			if err := c.Validate(); err == nil {
				t.Fatal("invalid config accepted")
			}
		})
	}
	for _, raw := range []string{`null`, `{}`, `{"token":"DO_NOT_ECHO"}`, strings.Repeat(" ", 65537), string(raw) + ` {}`} {
		if _, err := ReadConfig(strings.NewReader(raw)); err == nil || strings.Contains(err.Error(), "DO_NOT_ECHO") {
			t.Fatal("invalid or leaking config result", err)
		}
	}
}
