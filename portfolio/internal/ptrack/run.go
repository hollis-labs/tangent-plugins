package ptrack

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
)

// Config explicitly selects a backend; it carries no caller authority.
type Config struct {
	Version int    `json:"version"`
	Backend string `json:"backend"`
	Node    struct {
		Executable string `json:"executable"`
		Script     string `json:"script"`
	} `json:"node"`
	Plugin struct {
		BaseURL string `json:"base_url"`
	} `json:"plugin"`
}

// Options supplies process I/O and optional source-test transport injection.
// Client is never selected by a shipped config or an identity label.
type Options struct {
	Stdin     io.Reader
	Stdout    io.Writer
	Stderr    io.Writer
	LookupEnv func(string) string
	Client    Client
}

const help = `ptrack - portfolio API client

  list <db> [--status x] [--field k=v ...] [--sort f] [--text]
  get <db> <id> | schema <db> | databases | search <q> [--db x] | contract
  add <db> --title "..." [--field k=v ...] [--json '<item>']
  update <db> <id> [--set k=v ...] [--json '<patch>'] [--rev N]
  comment <db> <id> "text" [--kind decision]
  link|unlink <db:id> <db:id> [--field f]
  link-add <db:id> --kind k --ref r [--label l]
  link-remove <db:id> --kind k --ref r
  reorder <db> <id> <id> ...
  decide <DEC-id> --option <id> [--comment "..."]
  defer <DEC-id> [--note "..."] | reopen <DEC-id>
  inbox add "title" [--path p] [--kind k] [--body "..."]
  inbox promote <IN-id> --to <db> [--field k=v ...]
  inbox dismiss <IN-id> [--note "..."]
  torque task <id> | titles <id> ... | tasks | projects | epics | sprints | facets
  torque filters: --status --project --epic --sprint --priority --tag --q
                  --sort --dir --limit --offset --cursor --tags --dimensions
  board [--recent-hours N] [--limit N] [--text]
  migrate (administrative, explicit Node mode only)
  edge list <db:id> [--type TYPE] | backlinks <target-id> [--type TYPE]
  edge add|remove <db:id> <target-id> --type TYPE [--to-db DB] [--rev N]
  edge decision-gates <db:id>

  --as <label> attribution only; --text table output
  --project <msg://project/...> | --workstream <WS-...> portfolio query scope
  Torque --project keeps its upstream meaning; --scope-project selects portfolio scope.
  --config <file> or PTRACK_CONFIG: explicit version 1 backend node/plugin
  --backend node|plugin overrides only the configured transport.
  Unconfigured and unprovisioned plugin calls refuse. Exit codes: 0 ok, 1 error.
`

// Run maps and dispatches once. Explicit legacy children retain their exit code.
func Run(ctx context.Context, args []string, options Options) int {
	if options.Stdout == nil {
		options.Stdout = io.Discard
	}
	if options.Stderr == nil {
		options.Stderr = io.Discard
	}
	if options.LookupEnv == nil {
		options.LookupEnv = func(string) string { return "" }
	}
	a, err := parse(args)
	if err != nil {
		return report(options.Stderr, err)
	}
	if a.help() && !a.has("config") && options.LookupEnv("PTRACK_CONFIG") == "" && !a.has("backend") {
		if err = writeOutput(options.Stdout, help); err != nil {
			return report(options.Stderr, fail("unavailable", "stdout unavailable; no complete receipt was written"))
		}
		return 0
	}
	config, err := loadConfig(a, options.LookupEnv)
	if err != nil {
		return report(options.Stderr, err)
	}
	if config.Backend == "node" {
		if err = rejectNodeScope(a); err != nil {
			return report(options.Stderr, err)
		}
		// Explicit paths are operator-selected execution configuration, not a
		// tool argument or a discovered program on PATH.
		// Manager 0106 review18:08: explicit local execution configuration is
		// trusted to choose a runtime/script; absolute paths are not taint proof.
		// #nosec G204 -- reviewed operator-selected execution config, never API input/identity; no PATH discovery.
		child := exec.CommandContext(ctx, config.Node.Executable, append([]string{config.Node.Script}, a.child...)...)
		child.Stdin, child.Stdout, child.Stderr = options.Stdin, options.Stdout, options.Stderr
		if err = child.Run(); err == nil {
			return 0
		}
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			if code := exit.ExitCode(); code >= 0 {
				return code
			}
			return 1
		}
		return report(options.Stderr, fail("unavailable", "explicit Node backend unavailable; receipt may be incomplete, reconcile effects before retrying"))
	}
	if a.help() {
		if err = writeOutput(options.Stdout, help); err != nil {
			return report(options.Stderr, fail("unavailable", "stdout unavailable; no complete receipt was written"))
		}
		return 0
	}
	name, input, err := plan(a)
	if err != nil {
		return report(options.Stderr, err)
	}
	if name == "migrate" {
		return report(options.Stderr, fail("unsupported", "administrative migrate is only available in explicit Node mode"))
	}
	raw, err := json.Marshal(input)
	if err != nil || len(raw) > maxInput {
		return report(options.Stderr, fail("bad_request", "plugin input exceeds 32 KiB or cannot be encoded"))
	}
	client := options.Client
	if client == nil {
		client, err = newHTTPClient(config.Plugin.BaseURL)
		if err != nil {
			return report(options.Stderr, err)
		}
	}
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	result, err := client.Call(ctx, name, raw)
	if err != nil {
		return report(options.Stderr, err)
	}
	if err = writeResult(options.Stdout, name, result, a.has("text")); err != nil {
		return report(options.Stderr, fail("unavailable", "stdout or result unavailable; effects may have committed, reconcile before retrying"))
	}
	return 0
}

func rejectNodeScope(a arguments) error {
	if len(a.pos) == 0 {
		return nil
	}
	name := a.pos[0]
	if name == "edge" {
		return fail("unsupported", "typed edges are unavailable in legacy Node mode")
	}
	if name == "torque" && len(a.pos) > 1 {
		name = "torque_" + a.pos[1]
	}
	scope, err := selectedScope(a, name)
	if err != nil {
		return err
	}
	if scope != nil {
		return fail("unsupported", "portfolio scopes are unavailable in legacy Node mode")
	}
	return nil
}

func loadConfig(a arguments, lookup func(string) string) (Config, error) {
	var config Config
	path := a.flag("config")
	if !a.has("config") {
		path = lookup("PTRACK_CONFIG")
	}
	if path == "" {
		return config, fail("unavailable", "configure an explicit backend with --config or PTRACK_CONFIG; plugin caller provisioning is separate")
	}
	// Manager 0106 review18:08: arbitrary explicitly selected local config paths
	// are intentional; bounded JSON only, no implicit HOME/credential discovery.
	// #nosec G304 -- reviewed local --config/PTRACK_CONFIG input, not a server/tool path.
	file, err := os.Open(path)
	if err != nil {
		return config, fail("bad_request", "cannot read explicit ptrack config")
	}
	defer func() { _ = file.Close() }()
	raw, err := io.ReadAll(io.LimitReader(file, maxInput+1))
	if err != nil || len(raw) > maxInput {
		return config, fail("bad_request", "ptrack config exceeds 32 KiB or cannot be read")
	}
	if _, err = decodeJSON(raw); err != nil {
		return config, fail("bad_request", "ptrack config must be unambiguous JSON")
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if err = d.Decode(&config); err != nil || config.Version != 1 {
		return config, fail("bad_request", "ptrack config requires the version 1 contract")
	}
	if a.has("backend") {
		config.Backend = a.flag("backend")
	}
	switch config.Backend {
	case "node":
		if !filepath.IsAbs(config.Node.Executable) || !filepath.IsAbs(config.Node.Script) {
			return config, fail("bad_request", "Node backend requires explicit absolute executable and script paths")
		}
	case "plugin":
		if _, err = newHTTPClient(config.Plugin.BaseURL); err != nil {
			return config, err
		}
	default:
		return config, fail("bad_request", "ptrack backend must be node or plugin")
	}
	return config, nil
}

func report(out io.Writer, err error) int {
	var typed *Error
	if !errors.As(err, &typed) {
		typed = fail("unavailable", "ptrack operation unavailable")
	}
	raw, encodeErr := json.Marshal(object{"error": typed})
	if encodeErr != nil {
		raw = []byte(`{"error":{"code":"unavailable","message":"ptrack operation unavailable","details":{}}}`)
	}
	fmt.Fprintln(out, string(raw))
	return 1
}
