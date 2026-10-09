// Package messaging consumes committed Tether publications into Tangent.
// Concrete state belongs to the plugin's private Init.DataDir.
package messaging

import (
	"encoding/json"
	"errors"
	"io"
	"net/url"
	"path/filepath"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	gomsg "github.com/hollis-labs/go-messaging"
)

var channelName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

// Config contains non-secret operator inputs. No endpoint, caller or channel
// is inferred from message content or from an ambient home directory.
type Config struct {
	SchemaVersion    int           `json:"schema_version"`
	EndpointRef      string        `json:"endpoint_ref"`
	TetherAddress    string        `json:"tether_address"`
	CallerURN        string        `json:"caller_urn"`
	Channels         []string      `json:"channels"`
	RequestTimeoutMS int           `json:"request_timeout_ms"`
	ReconnectMinMS   int           `json:"reconnect_min_ms"`
	ReconnectMaxMS   int           `json:"reconnect_max_ms"`
	HistoryLimit     int           `json:"history_limit"`
	Stages           []StageConfig `json:"stages"`
}

// StageConfig selects the independently owned builtin summarizer. It carries
// non-secret endpoint/model and immutable instruction inputs, never a session,
// tool declaration, provider key, or implicit agent execution configuration.
type StageConfig struct {
	ID              string `json:"id"`
	Priority        int    `json:"priority"`
	EndpointURL     string `json:"endpoint_url"`
	CallerID        string `json:"caller_id"`
	ProviderHint    string `json:"provider_hint,omitempty"`
	ModelHint       string `json:"model_hint,omitempty"`
	TimeoutMS       int    `json:"timeout_ms"`
	InstructionPath string `json:"instruction_path,omitempty"`
}

// ReadConfig rejects unknown fields, oversized documents and trailing input.
// Errors deliberately omit operator input and document contents.
func ReadConfig(reader io.Reader) (Config, error) {
	var config Config
	raw, err := io.ReadAll(io.LimitReader(reader, 65537))
	if err != nil || len(raw) > 65536 {
		return config, errors.New("messaging: configuration unreadable or oversized")
	}
	if !strictJSON(raw) {
		return Config{}, errors.New("messaging: invalid configuration")
	}
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.DisallowUnknownFields()
	if err = decoder.Decode(&config); err != nil {
		return Config{}, errors.New("messaging: invalid configuration")
	}
	if err = decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		return Config{}, errors.New("messaging: trailing configuration")
	}
	if err = config.Validate(); err != nil {
		return Config{}, err
	}
	config.Channels = append([]string(nil), config.Channels...)
	config.Stages = append([]StageConfig(nil), config.Stages...)
	return config, nil
}

// Validate requires explicit source identity and finite transport bounds.
func (c Config) Validate() error {
	if c.SchemaVersion != 1 || !identifier(c.EndpointRef, 256) {
		return errors.New("messaging: invalid configuration identity")
	}
	if _, err := gomsg.ParseURN(c.CallerURN); err != nil {
		return errors.New("messaging: invalid caller identity")
	}
	if strings.HasPrefix(c.TetherAddress, "unix:") {
		if !filepath.IsAbs(strings.TrimPrefix(c.TetherAddress, "unix:")) {
			return errors.New("messaging: explicit absolute Tether socket required")
		}
	} else {
		u, err := url.Parse(c.TetherAddress)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
			return errors.New("messaging: invalid Tether endpoint")
		}
	}
	if len(c.Channels) == 0 || len(c.Channels) > 8 {
		return errors.New("messaging: explicit bounded channels required")
	}
	seen := make(map[string]bool)
	for _, channel := range c.Channels {
		if !channelName.MatchString(channel) || seen[channel] {
			return errors.New("messaging: invalid or duplicate channel")
		}
		seen[channel] = true
	}
	if c.RequestTimeoutMS < 1 || c.RequestTimeoutMS > 60000 || c.ReconnectMinMS < 1 || c.ReconnectMaxMS < c.ReconnectMinMS || c.ReconnectMaxMS > 60000 || c.HistoryLimit < 1 || c.HistoryLimit > 1000 {
		return errors.New("messaging: invalid finite transport limits")
	}
	if len(c.Stages) != 1 {
		return errors.New("messaging: explicit_builtin_stage_required")
	}
	stage := c.Stages[0]
	u, err := url.Parse(stage.EndpointURL)
	validEndpoint := err == nil && (u.Scheme == "http" || u.Scheme == "https") && u.Host != "" && u.User == nil && u.RawQuery == "" && u.Fragment == ""
	if strings.HasPrefix(stage.EndpointURL, "unix:") {
		validEndpoint = filepath.IsAbs(strings.TrimPrefix(stage.EndpointURL, "unix:"))
	}
	if stage.ID != "summarize" || !identifier(stage.CallerID, 128) || !validEndpoint || stage.TimeoutMS < 0 || stage.TimeoutMS > 60000 {
		return errors.New("messaging: invalid_builtin_stage")
	}
	for _, hint := range []string{stage.ProviderHint, stage.ModelHint} {
		if hint != "" && !identifier(hint, 128) {
			return errors.New("messaging: invalid_stage_hint")
		}
	}
	if stage.InstructionPath != "" && (!filepath.IsAbs(stage.InstructionPath) || !identifier(stage.InstructionPath, 4096)) {
		return errors.New("messaging: explicit_instruction_path_required")
	}
	return nil
}

// RequestTimeout bounds a single history, capability or sink request.
func (c Config) RequestTimeout() time.Duration {
	return time.Duration(c.RequestTimeoutMS) * time.Millisecond
}

func identifier(value string, limit int) bool {
	if value == "" || !utf8.ValidString(value) || len(value) > limit || strings.TrimSpace(value) != value {
		return false
	}
	for _, r := range value {
		if r < 32 || r == 127 {
			return false
		}
	}
	return true
}
