package main

import (
	"encoding/json"
	"errors"
	"os"
	"strconv"
	"strings"

	"github.com/hollis-labs/tangent-plugins/messaging/internal/messaging"
)

// resolveConfiguration is the sole domain decoder for manifest scalar settings.
// The host transports declared strings and never interprets channels or stages.
func resolveConfiguration(inputs map[string]string) (messaging.Config, string, error) {
	if len(inputs) == 0 {
		config, err := readConfiguration(os.Getenv(configEnv))
		return config, os.Getenv("TETHER_TOKEN"), err
	}
	mode := inputs["configuration_mode"]
	if mode == "" {
		mode = "file"
	} // Legacy explicit host path contract.
	if mode == "file" {
		provided := map[string]string{}
		for key, value := range inputs {
			if key != "configuration_mode" {
				provided[key] = value
			}
		}
		path, token, err := configurationInputs(provided)
		// Provided configuration must never become standalone mode after removing mode.
		if len(provided) == 0 || err != nil {
			return messaging.Config{}, "", errors.New("messaging: host_configuration_refused")
		}
		config, err := readConfiguration(path)
		return config, token, err
	}
	if mode != "settings" {
		return messaging.Config{}, "", errors.New("messaging: host_configuration_refused")
	}
	known := map[string]bool{"configuration_mode": true, "endpoint_ref": true, "tether_address": true, "caller_urn": true, "channels_json": true, "stages_json": true, "instruction_path": true, "request_timeout_ms": true, "reconnect_min_ms": true, "reconnect_max_ms": true, "history_limit": true, "tether_token": true}
	for key := range inputs {
		if !known[key] {
			return messaging.Config{}, "", errors.New("messaging: host_configuration_refused")
		}
	}
	config := messaging.Config{SchemaVersion: 1, EndpointRef: inputs["endpoint_ref"], TetherAddress: inputs["tether_address"], CallerURN: inputs["caller_urn"]}
	for key, target := range map[string]*int{"request_timeout_ms": &config.RequestTimeoutMS, "reconnect_min_ms": &config.ReconnectMinMS, "reconnect_max_ms": &config.ReconnectMaxMS, "history_limit": &config.HistoryLimit} {
		n, err := strconv.Atoi(inputs[key])
		if err != nil {
			return messaging.Config{}, "", errors.New("messaging: invalid_host_settings")
		}
		*target = n
	}
	// Embed the raw lists into the existing strict domain codec. Its duplicate,
	// UTF-8, unknown-property and size validation applies before typed decoding.
	raw, err := json.Marshal(config)
	if err != nil {
		return messaging.Config{}, "", errors.New("messaging: invalid_host_settings")
	}
	var document map[string]json.RawMessage
	if err = json.Unmarshal(raw, &document); err != nil {
		return messaging.Config{}, "", err
	}
	document["channels"] = json.RawMessage(inputs["channels_json"])
	document["stages"] = json.RawMessage(inputs["stages_json"])
	raw, err = json.Marshal(document)
	if err != nil {
		return messaging.Config{}, "", errors.New("messaging: invalid_host_settings")
	}
	config, err = messaging.ReadConfig(strings.NewReader(string(raw)))
	if err != nil {
		return messaging.Config{}, "", err
	}
	if instruction, provided := inputs["instruction_path"]; provided {
		if config.Stages[0].InstructionPath != "" {
			return messaging.Config{}, "", errors.New("messaging: ambiguous_instruction_path")
		}
		config.Stages[0].InstructionPath = instruction
		if err = config.Validate(); err != nil {
			return messaging.Config{}, "", err
		}
	}
	return config, inputs["tether_token"], nil
}
