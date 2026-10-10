// Package integration verifies packaged source artifacts with a private host.
// It never uses an installed daemon, operator data or a backend/keychain value.
package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/cookiejar"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/hollis-labs/tangent/pkg/plugin"
)

const (
	pluginID   = "tangent.plugin.chat-agent"
	tool       = "tangent.chat_agent_status"
	route      = "/api/plugins/chat-agent/status"
	management = "/api/plugin-management/" + pluginID
)

type fixture struct {
	binary, root, bundle string
	process              *os.Process
	done                 chan error
	log                  *os.File
	url                  string
	client               *http.Client
}

func build(t *testing.T, dir, target, source string) {
	t.Helper()
	// Builds accept only these literal source targets; outputs belong to t.TempDir.
	if source != "./cmd/tangent" && source != "./cmd/tangent-plugin-chat-agent" {
		t.Fatal("unsupported build target")
	}
	executable, err := exec.LookPath("go")
	if err != nil {
		t.Fatal(err)
	}
	log, err := os.CreateTemp(filepath.Dir(target), "build-log-")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = log.Close(); _ = os.Remove(log.Name()) }()
	process, err := os.StartProcess(executable, []string{executable, "build", "-mod=readonly", "-o", target, source}, &os.ProcAttr{Dir: dir, Env: os.Environ(), Files: []*os.File{nil, log, log}})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		state, e := process.Wait()
		if e == nil && !state.Success() {
			e = fmt.Errorf("build exited: %s", state)
		}
		done <- e
	}()
	select {
	case err = <-done:
	case <-time.After(2 * time.Minute):
		_ = process.Kill()
		<-done
		t.Fatal("build deadline exceeded")
	}
	if _, e := log.Seek(0, io.SeekStart); e != nil {
		t.Fatal(e)
	}
	raw, e := io.ReadAll(log)
	if e != nil {
		t.Fatal(e)
	}
	if err != nil {
		t.Fatalf("build: %v\n%s", err, raw)
	}
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	dir := t.TempDir()
	f := &fixture{binary: filepath.Join(dir, "tangent"), root: filepath.Join(dir, "operator"), bundle: filepath.Join(dir, "bundle")}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	locate := exec.CommandContext(ctx, "go", "list", "-m", "-f", "{{.Dir}}", "github.com/hollis-labs/tangent")
	raw, err := locate.Output()
	if err != nil {
		t.Fatal(err)
	}
	build(t, strings.TrimSpace(string(raw)), f.binary, "./cmd/tangent")
	payload := filepath.Join(f.bundle, "bin", "tangent-plugin-chat-agent")
	if err = os.MkdirAll(filepath.Dir(payload), 0700); err != nil {
		t.Fatal(err)
	}
	build(t, "..", payload, "./cmd/tangent-plugin-chat-agent")
	// Only --manifest executes here; it must inventory without touching a backend.
	if err = os.MkdirAll(f.root, 0700); err != nil {
		t.Fatal(err)
	}
	declaration := f.run(t, payload, []string{"--manifest"})
	manifest, err := plugin.DecodeManifest(bytes.NewReader(declaration))
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(f.bundle, plugin.ManifestName), declaration, 0600); err != nil {
		t.Fatal(err)
	}
	if err = manifest.VerifyBundle(f.bundle); err != nil {
		t.Fatal(err)
	}
	f.run(t, f.binary, []string{"plugin", "install", f.bundle})
	t.Cleanup(func() { f.stop(t) })
	return f
}
func (f *fixture) env() []string {
	return []string{"HOME=" + f.root, "XDG_CONFIG_HOME=" + f.root, "XDG_DATA_HOME=" + f.root, "TANGENT_DB_PATH=" + filepath.Join(f.root, "host.sqlite3"), "TANGENT_PLUGIN_DIR=" + filepath.Join(f.root, "plugins"), "TANGENT_OTEL=0", "TMPDIR=" + os.Getenv("TMPDIR")}
}

// The executable paths are constructed exclusively by this fixture's builds.
// StartProcess lets the test own/reap the actual host and CLI without a shell.
func (f *fixture) run(t *testing.T, executable string, args []string) []byte {
	t.Helper()
	log, err := os.CreateTemp(f.root, "command-")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = log.Close() }()
	process, err := os.StartProcess(executable, append([]string{executable}, args...), &os.ProcAttr{Env: f.env(), Files: []*os.File{nil, log, log}})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		state, e := process.Wait()
		if e == nil && !state.Success() {
			e = fmt.Errorf("command exited: %s", state)
		}
		done <- e
	}()
	select {
	case err = <-done:
	case <-time.After(30 * time.Second):
		_ = process.Kill()
		<-done
		t.Fatal("fixture command exceeded deadline")
	}
	if _, errSeek := log.Seek(0, io.SeekStart); errSeek != nil {
		t.Fatal(errSeek)
	}
	raw, readErr := io.ReadAll(log)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if err != nil {
		t.Fatalf("private command: %v\n%s", err, raw)
	}
	return raw
}
func (f *fixture) start(t *testing.T) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	_ = listener.Close()
	f.url = "http://127.0.0.1:" + strconv.Itoa(port)
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	f.client = &http.Client{Jar: jar, Timeout: 3 * time.Second}
	f.log, err = os.CreateTemp(f.root, "host-log-")
	if err != nil {
		t.Fatal(err)
	}
	f.process, err = os.StartProcess(f.binary, []string{f.binary}, &os.ProcAttr{Env: append(f.env(), "TANGENT_HTTP_PORT="+strconv.Itoa(port)), Files: []*os.File{nil, f.log, f.log}})
	if err != nil {
		t.Fatal(err)
	}
	f.done = make(chan error, 1)
	process, done := f.process, f.done
	go func() {
		state, e := process.Wait()
		if e == nil && !state.Success() {
			e = fmt.Errorf("host exited: %s", state)
		}
		done <- e
	}()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		req, e := http.NewRequest(http.MethodGet, f.url+"/", nil)
		if e != nil {
			t.Fatal(e)
		}
		req.Header.Set("Accept", "text/html")
		response, e := f.client.Do(req)
		if e == nil {
			_ = response.Body.Close()
			if response.StatusCode == 200 {
				return
			}
		}
		select {
		case e = <-f.done:
			f.process = nil
			t.Fatal("private host startup", e)
		case <-time.After(20 * time.Millisecond):
		}
	}
	t.Fatal("private host did not start")
}
func (f *fixture) stop(t *testing.T) {
	t.Helper()
	if f.process == nil {
		return
	}
	_ = f.process.Signal(os.Interrupt)
	select {
	case err := <-f.done:
		if err != nil {
			t.Error(err)
		}
	case <-time.After(8 * time.Second):
		_ = f.process.Kill()
		<-f.done
		t.Error("host required containment")
	}
	f.process = nil
	if err := f.log.Close(); err != nil {
		t.Error(err)
	}
}
func (f *fixture) request(t *testing.T, method, path string, body any) (int, []byte) {
	t.Helper()
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		reader = bytes.NewReader(raw)
	}
	req, err := http.NewRequest(method, f.url+path, reader)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Origin", f.url)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	response, err := f.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = response.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		t.Fatal(err)
	}
	return response.StatusCode, raw
}
func (f *fixture) require(t *testing.T, method, path string, body any, target any) []byte {
	t.Helper()
	code, raw := f.request(t, method, path, body)
	if code != 200 {
		t.Fatalf("%s %s: %d %s", method, path, code, raw)
	}
	if target != nil {
		if err := json.Unmarshal(raw, target); err != nil {
			t.Fatal(err)
		}
	}
	return raw
}

type snapshot struct {
	Revision string `json:"revision"`
	Pending  bool   `json:"pending_restart"`
	Values   map[string]struct {
		Value any `json:"value"`
	} `json:"values"`
}

func (f *fixture) config(t *testing.T) snapshot {
	t.Helper()
	var value snapshot
	f.require(t, "GET", management+"/config?scope_kind=client&scope_id=local", nil, &value)
	return value
}
func settingsRequest(revision string, set map[string]any, unset []string) map[string]any {
	return map[string]any{"scope": map[string]string{"kind": "client", "id": "local"}, "revision": revision, "changes": map[string]any{"set": set, "unset": unset}}
}
func (f *fixture) checkContributions(t *testing.T, enabled bool) {
	t.Helper()
	var inventory struct {
		Tools   []string `json:"contributed_tools"`
		Routes  []string `json:"contributed_routes"`
		Plugins []struct {
			ID      string `json:"id"`
			Enabled bool   `json:"enabled"`
			Loaded  bool   `json:"loaded"`
		} `json:"plugins"`
	}
	f.require(t, "GET", "/api/plugin-management", nil, &inventory)
	if slices.Contains(inventory.Tools, tool) != enabled || slices.Contains(inventory.Routes, "GET "+route) != enabled {
		t.Fatal("contributions disagree with lifecycle", inventory)
	}
	found := false
	for _, record := range inventory.Plugins {
		if record.ID == pluginID {
			found = true
			if record.Enabled != enabled || record.Loaded != enabled {
				t.Fatal("owner state", record)
			}
		}
	}
	if !found {
		t.Fatal("missing installed owner")
	}
	raw := f.require(t, "POST", "/mcp", map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/list", "params": map[string]any{}}, nil)
	if bytes.Contains(raw, []byte(tool)) != enabled {
		t.Fatal("live MCP registry did not withdraw/restore tool")
	}
	if enabled {
		var call struct {
			Result struct {
				IsError bool `json:"isError"`
				Content []struct {
					Text string `json:"text"`
				} `json:"content"`
			} `json:"result"`
		}
		f.require(t, "POST", "/mcp", map[string]any{"jsonrpc": "2.0", "id": 2, "method": "tools/call", "params": map[string]any{"name": tool, "arguments": map[string]any{}}}, &call)
		if call.Result.IsError || len(call.Result.Content) != 1 || !strings.Contains(call.Result.Content[0].Text, `"chat_available":false`) {
			t.Fatal("real MCP dispatch", call)
		}
	}
	code, raw := f.request(t, "GET", route, nil)
	if enabled {
		if code != 200 || !bytes.Contains(raw, []byte(`"chat_available":false`)) || !bytes.Contains(raw, []byte(`"agent_running":false`)) {
			t.Fatal(code, string(raw))
		}
	} else if code < 400 {
		t.Fatal("disabled route still dispatched", code, string(raw))
	}
	if !enabled {
		runtimeDir := filepath.Join(f.root, "plugins", ".runtime")
		entries, err := os.ReadDir(runtimeDir)
		if err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
		if len(entries) != 0 {
			t.Fatal("disable retained executable snapshots", entries)
		}
	}
}

func TestPackagedLifecycleSettingsAndDisabledRestart(t *testing.T) {
	f := newFixture(t)
	f.start(t)
	// Current public host auto-enables unknown IDs. This demonstrates lifecycle,
	// not default-off installation: that prerequisite is explicitly still unmet.
	f.checkContributions(t, true)
	var groups struct {
		Groups []struct {
			ID     string `json:"id"`
			Schema struct {
				Properties map[string]struct {
					Type    string   `json:"type"`
					Enum    []string `json:"enum"`
					Default any      `json:"default"`
				} `json:"properties"`
			} `json:"schema"`
		} `json:"groups"`
	}
	f.require(t, "GET", "/api/plugin-management/config", nil, &groups)
	if len(groups.Groups) != 1 || groups.Groups[0].ID != pluginID {
		t.Fatal("settings group missing", groups)
	}
	fields := groups.Groups[0].Schema.Properties
	if fields["rail_open"].Type != "boolean" || fields["rail_open"].Default != false || !slices.Contains(fields["density"].Enum, "compact") {
		t.Fatal("typed settings form projection", fields)
	}
	before := f.config(t)
	update := settingsRequest(before.Revision, map[string]any{"conversation_ref": "synthetic-conversation", "agent_ref": "synthetic-agent", "rail_open": true, "density": "compact"}, []string{})
	f.require(t, "POST", management+"/config/validate", update, nil)
	var saved snapshot
	f.require(t, "POST", management+"/config/save", update, &saved)
	if !saved.Pending {
		t.Fatal("save silently applied")
	}
	if code, _ := f.request(t, "POST", management+"/config/save", update); code != 409 {
		t.Fatal("stale revision accepted", code)
	}
	// The running owner keeps its detached snapshot until explicit apply.
	raw := f.require(t, "GET", route, nil, nil)
	if !bytes.Contains(raw, []byte(`"density":"comfortable"`)) {
		t.Fatal("save mutated running snapshot", string(raw))
	}
	f.require(t, "POST", management+"/config/apply", settingsRequest(saved.Revision, map[string]any{}, []string{}), nil)
	raw = f.require(t, "GET", route, nil, nil)
	if !bytes.Contains(raw, []byte(`"density":"compact"`)) || !bytes.Contains(raw, []byte(`"conversation_configured":true`)) || bytes.Contains(raw, []byte("synthetic-conversation")) {
		t.Fatal("apply snapshot or private reference", string(raw))
	}
	// Only declared scalar preferences/references can enter the host settings DB.
	for _, set := range []map[string]any{{"history": "transcript"}, {"backend_token": "synthetic-token"}, {"density": "bad"}, {"rail_open": "true"}} {
		current := f.config(t)
		code, _ := f.request(t, "POST", management+"/config/save", settingsRequest(current.Revision, set, []string{}))
		if code < 400 {
			t.Fatal("invalid settings accepted", set)
		}
	}
	current := f.config(t)
	f.require(t, "POST", management+"/config/reset", settingsRequest(current.Revision, map[string]any{}, []string{"rail_open"}), nil)
	f.require(t, "POST", management+"/disable", map[string]any{}, nil)
	f.checkContributions(t, false)
	if code, _ := f.request(t, "POST", management+"/config/apply", settingsRequest(f.config(t).Revision, map[string]any{}, []string{})); code != 409 {
		t.Fatal("apply enabled disabled plugin", code)
	}
	f.stop(t)
	f.start(t)
	f.checkContributions(t, false)
	f.require(t, "POST", management+"/enable", map[string]any{}, nil)
	f.checkContributions(t, true)
	raw = f.require(t, "GET", route, nil, nil)
	if !bytes.Contains(raw, []byte(`"rail_open":false`)) || !bytes.Contains(raw, []byte(`"density":"compact"`)) {
		t.Fatal("saved preference/reset lost at restart", string(raw))
	}
	f.require(t, "POST", management+"/reload", map[string]any{}, nil)
	f.checkContributions(t, true)
	f.require(t, "POST", management+"/disable", map[string]any{}, nil)
	f.checkContributions(t, false)
	t.Log("packaged install; host settings projection, validation/CAS/save/apply/reset; owner withdrawal; disabled restart; enable/reload; no backend or UI integration")
}
