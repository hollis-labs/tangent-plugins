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
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	gomsg "github.com/hollis-labs/go-messaging"
	tether "github.com/hollis-labs/go-tether-client"
	pluginhost "github.com/hollis-labs/libs/plugin-mcp/plugin-host"
	"github.com/hollis-labs/libs/plugin-mcp/plugin-sdk/capability"
	"github.com/hollis-labs/libs/plugin-mcp/plugin-sdk/subprocess"
	"github.com/hollis-labs/tangent-plugins/messaging/internal/messaging"
	"github.com/hollis-labs/tangent/pkg/plugin"
)

const channel = "stage1"

var binaries struct {
	sync.Once
	dir, host, consumer string
	err                 error
}

func TestMain(m *testing.M) {
	code := m.Run()
	if binaries.dir != "" {
		_ = os.RemoveAll(binaries.dir)
	}
	os.Exit(code)
}
func buildBinaries(t *testing.T) (string, string) {
	t.Helper()
	binaries.Do(func() {
		binaries.dir, binaries.err = os.MkdirTemp("", "messaging-stage1-build-")
		if binaries.err != nil {
			return
		}
		binaries.host = filepath.Join(binaries.dir, "tangent")
		binaries.consumer = filepath.Join(binaries.dir, "messaging")
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		locate := exec.CommandContext(ctx, "go", "list", "-m", "-f", "{{.Dir}}", "github.com/hollis-labs/tangent")
		raw, err := locate.Output()
		if err != nil {
			binaries.err = err
			return
		}
		for _, build := range []struct{ dir, target, source string }{
			{strings.TrimSpace(string(raw)), binaries.host, "./cmd/tangent"},
			{"..", binaries.consumer, "./cmd/tangent-plugin-messaging"},
		} {
			cmd := exec.CommandContext(ctx, "go", "build", "-race", "-mod=readonly", "-o", build.target, build.source) // #nosec G204 -- literal Go packages above, output inside this fixture's private build directory.
			cmd.Dir = build.dir
			if output, e := cmd.CombinedOutput(); e != nil {
				binaries.err = fmt.Errorf("build %s: %w\n%s", build.source, e, output)
				return
			}
		}
	})
	if binaries.err != nil {
		t.Fatal(binaries.err)
	}
	return binaries.host, binaries.consumer
}

type lockedLog struct {
	sync.Mutex
	bytes.Buffer
}

func (l *lockedLog) Write(p []byte) (int, error) {
	l.Lock()
	defer l.Unlock()
	return l.Buffer.Write(p)
}
func (l *lockedLog) text() string { l.Lock(); defer l.Unlock(); return l.String() }

type host struct {
	url    string
	client *http.Client
	cmd    *exec.Cmd
	done   chan error
	logs   lockedLog
}

func startHost(t *testing.T) *host {
	t.Helper()
	binary, _ := buildBinaries(t)
	root := t.TempDir()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	_ = listener.Close()
	h := &host{url: fmt.Sprintf("http://127.0.0.1:%d", port), done: make(chan error, 1)}
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	h.client = &http.Client{Jar: jar, Timeout: 2 * time.Second}
	h.cmd = exec.Command(binary) // #nosec G204 -- exact published host command built by this test, never an operator-installed binary.
	h.cmd.Env = []string{"HOME=" + root, "XDG_CONFIG_HOME=" + root, "XDG_DATA_HOME=" + root, "TANGENT_DB_PATH=" + filepath.Join(root, "host.sqlite3"), "TANGENT_PLUGIN_DIR=" + filepath.Join(root, "plugins"), "TANGENT_HTTP_PORT=" + strconv.Itoa(port), "TANGENT_OTEL=0"}
	h.cmd.Stdout = &h.logs
	h.cmd.Stderr = &h.logs
	if err = h.cmd.Start(); err != nil {
		t.Fatal(err)
	}
	go func() { h.done <- h.cmd.Wait() }()
	t.Cleanup(func() {
		_ = h.cmd.Process.Signal(os.Interrupt)
		select {
		case e := <-h.done:
			if e != nil {
				t.Error("host exit", e, h.logs.text())
			}
		case <-time.After(5 * time.Second):
			_ = h.cmd.Process.Kill()
			<-h.done
			t.Error("host cleanup required containment")
		}
	})
	await(t, func() bool {
		req, e := http.NewRequest(http.MethodGet, h.url+"/", nil)
		if e != nil {
			return false
		}
		req.Header.Set("Accept", "text/html")
		response, e := h.client.Do(req)
		if e != nil {
			return false
		}
		_ = response.Body.Close()
		return response.StatusCode == 200
	}, "private host document/session ready")
	return h
}
func await(t *testing.T, check func() bool, meaning string) {
	t.Helper()
	deadline := time.NewTimer(8 * time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	for {
		if check() {
			return
		}
		select {
		case <-tick.C:
		case <-deadline.C:
			t.Fatal("not reached:", meaning)
		}
	}
}
func (h *host) request(t *testing.T, method, path string, body any, target any) int {
	t.Helper()
	var reader io.Reader
	if body != nil {
		raw, e := json.Marshal(body)
		if e != nil {
			t.Fatal(e)
		}
		reader = bytes.NewReader(raw)
	}
	req, e := http.NewRequest(method, h.url+path, reader)
	if e != nil {
		t.Fatal(e)
	}
	req.Header.Set("Origin", h.url)
	req.Header.Set("Content-Type", "application/json")
	response, e := h.client.Do(req)
	if e != nil {
		t.Fatal(e)
	}
	defer func() { _ = response.Body.Close() }()
	raw, e := io.ReadAll(io.LimitReader(response.Body, 2<<20))
	if e != nil {
		t.Fatal(e)
	}
	if target != nil && response.StatusCode == 200 {
		if e = json.Unmarshal(raw, target); e != nil {
			t.Fatal("host response", e, string(raw))
		}
	}
	return response.StatusCode
}

type item struct {
	ItemID        string                    `json:"item_id"`
	Revision      int64                     `json:"revision"`
	Content       string                    `json:"content"`
	Summary       string                    `json:"summary"`
	Kind          string                    `json:"kind"`
	TurnID        string                    `json:"turn_id"`
	SessionID     string                    `json:"session_id"`
	Replyable     bool                      `json:"replyable"`
	DeliveryState string                    `json:"delivery_state"`
	Source        *plugin.TurnSourceMessage `json:"source_message"`
	Trace         []plugin.TurnStageTrace   `json:"stage_trace"`
}

func (h *host) items(t *testing.T) map[string]item {
	t.Helper()
	var inbox struct{ Pending, History []item }
	if status := h.request(t, http.MethodGet, "/api/turns", nil, &inbox); status != 200 {
		t.Fatal("inbox", status, h.logs.text())
	}
	result := map[string]item{}
	for _, row := range append(inbox.Pending, inbox.History...) {
		if row.Source != nil {
			if _, duplicate := result[row.Source.MessageID]; duplicate {
				t.Fatal("duplicate publication item")
			}
			result[row.Source.MessageID] = row
		}
	}
	return result
}
func (h *host) inspect(t *testing.T, id string) item {
	t.Helper()
	var out item
	if status := h.request(t, http.MethodGet, "/api/turns/items/"+id, nil, &out); status != 200 {
		t.Fatal("inspect", status)
	}
	return out
}

type upstream struct {
	live          chan tether.ChannelMessage
	server        *httptest.Server
	mu            sync.Mutex
	messages      []tether.ChannelMessage
	ai            map[string]int
	sends         map[string]int
	keys          map[string]string
	delivered     bool
	deliveryReads int
	subscribed    int
	joined        int
}

func newUpstream(t *testing.T, messages []tether.ChannelMessage) *upstream {
	t.Helper()
	u := &upstream{live: make(chan tether.ChannelMessage, 1), messages: messages, ai: map[string]int{}, sends: map[string]int{}, keys: map[string]string{}}
	u.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/channels/"+channel+"/messages":
			since, _ := strconv.ParseInt(r.URL.Query().Get("since"), 10, 64)
			u.mu.Lock()
			var rows []tether.ChannelMessage
			for _, m := range u.messages {
				if m.Seq > since {
					rows = append(rows, m)
				}
			}
			u.mu.Unlock()
			writeJSON(t, w, tether.ChannelMessagesResponse{Channel: tether.Channel{Name: channel, Address: "msg://service/local/channel/" + channel}, Messages: rows, NextSince: 99999})
		case r.URL.Path == "/channels/"+channel+"/subscribe":
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = fmt.Fprint(w, ": private fixture stream\n\n")
			w.(http.Flusher).Flush()
			u.mu.Lock()
			u.subscribed++
			u.mu.Unlock()
			select {
			case m := <-u.live:
				raw, _ := json.Marshal(m)
				_, _ = fmt.Fprintf(w, "id: %d\nevent: message\ndata: %s\n\n", m.Seq, raw)
				w.(http.Flusher).Flush()
			case <-r.Context().Done():
			}
			u.mu.Lock()
			u.joined++
			u.mu.Unlock()
		case r.URL.Path == "/ai/chat":
			var q tether.ChatRequest
			if json.NewDecoder(r.Body).Decode(&q) != nil {
				t.Error("gateway request")
				return
			}
			if q.Request.Operation != "chat" || q.Request.Mode != "summarize" || len(q.Request.Tools) != 0 || len(q.Request.Attachments) != 0 || q.Request.SessionID != "" || len(q.Request.Input) != 2 || q.Request.MaxOutputTokens != 512 {
				t.Error("not tool-free bounded request")
			}
			data := strings.TrimPrefix(q.Request.Input[1].Parts[0].Text, "Untrusted source JSON data:\n")
			var source struct{ Original string }
			if json.Unmarshal([]byte(data), &source) != nil {
				t.Error("unquoted original")
			}
			u.mu.Lock()
			u.ai[source.Original]++
			u.mu.Unlock()
			switch source.Original {
			case "timeout":
				<-r.Context().Done()
				return
			case "refusal":
				w.WriteHeader(http.StatusForbidden)
				return
			}
			response := tether.ChatResponse{Response: tether.AIResponse{StopReason: "completed", Output: []tether.AIMessage{{Role: "assistant", Parts: []tether.AIContentPart{{Type: "text", Text: `{"summary":"Fixture summary"}`}}}}}}
			if source.Original == "tool" {
				response.Response.Output[0].ToolUse = &tether.AIToolUse{Name: "forbidden_fixture_tool"}
			}
			writeJSON(t, w, response)
		case r.URL.Path == "/routing/capabilities":
			writeJSON(t, w, tether.RoutingCapabilitiesResponse{SessionID: r.URL.Query().Get("session_id"), RouteSupported: true, ReplyToSender: true, Interrupt: false})
		case strings.HasSuffix(r.URL.Path, "/reply"):
			id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/messages/"), "/reply")
			var body struct {
				Body      string
				Interrupt bool
			}
			if json.NewDecoder(r.Body).Decode(&body) != nil || body.Interrupt || body.Body != "Operator fixture answer" {
				t.Error("unexpected reply/action")
			}
			key := r.Header.Get("Idempotency-Key")
			u.mu.Lock()
			u.sends[id]++
			previous := u.keys[id]
			if key == "" || (previous != "" && previous != key) {
				t.Error("unstable reply key")
			}
			u.keys[id] = key
			u.mu.Unlock()
			w.WriteHeader(http.StatusAccepted)
			writeJSON(t, w, tether.ReplyReceipt{ReplyID: "reply-" + id, ParentID: id, State: "queued", TargetSessionID: "fixture-session", Duplicate: previous != ""})
		case strings.HasSuffix(r.URL.Path, "/delivery"):
			id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/messages/reply-"), "/delivery")
			u.mu.Lock()
			u.deliveryReads++
			delivered := u.delivered
			u.mu.Unlock()
			state := tether.ReplyQueued
			if delivered {
				state = tether.ReplyDelivered
			}
			writeJSON(t, w, tether.ReplyDelivery{ReplyID: "reply-" + id, ParentID: id, State: state, OriginalSessionID: "fixture-session", TargetSessionID: "fixture-session", DeliveredToSessionID: map[bool]string{true: "fixture-session"}[delivered], Attempts: 1})
		default:
			t.Error("unexpected upstream effect", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(u.server.Close)
	return u
}
func writeJSON(t *testing.T, w http.ResponseWriter, value any) {
	t.Helper()
	if e := json.NewEncoder(w).Encode(value); e != nil {
		t.Error(e)
	}
}
func publication(seq int64, id, body, kind string) tether.ChannelMessage {
	raw, _ := json.Marshal(map[string]string{"text": body})
	from := gomsg.Address{Kind: gomsg.KindAgent, Authority: "local", ID: "fixture-publisher"}
	meta := map[string]string{}
	thread := ""
	if kind != "" {
		from = gomsg.Address{Kind: gomsg.KindSession, Authority: "local", ID: "fixture-session"}
		thread = "fixture-session"
		meta = map[string]string{"kind": kind, "session_id": thread, "turn_id": "same-fixture-turn", "output_id": id, "logical_agent_id": "fixture-agent"}
	}
	return tether.ChannelMessage{Seq: seq, Envelope: gomsg.Envelope{ID: id, Kind: gomsg.MsgKindNotice, From: from, To: gomsg.Address{Kind: gomsg.KindService, Authority: "local", ID: "channel", SubID: channel}, Channel: channel, ContentType: "application/json", Payload: raw, Metadata: meta, ThreadID: thread}}
}
func privateDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if e := os.Chmod(dir, 0700); e != nil { // #nosec G302 -- test-owned directory, not a file; private ledger requires 0700.
		t.Fatal(e)
	}
	return dir
}
func startPlugin(t *testing.T, u *upstream, mcpURL, data string, generation uint64) *pluginhost.Process {
	t.Helper()
	_, binary := buildBinaries(t)
	cache := privateDir(t)
	config := messaging.Config{SchemaVersion: 1, EndpointRef: "stage1-fixture", TetherAddress: u.server.URL, CallerURN: "msg://service/local/messaging", Channels: []string{channel}, RequestTimeoutMS: 2000, ReconnectMinMS: 25, ReconnectMaxMS: 50, HistoryLimit: 100, Stages: []messaging.StageConfig{{ID: "summarize", EndpointURL: u.server.URL, CallerID: "stage1-fixture", TimeoutMS: 200}}}
	raw, e := json.Marshal(config)
	if e != nil {
		t.Fatal(e)
	}
	path := filepath.Join(privateDir(t), "config.json")
	if e = os.WriteFile(path, raw, 0600); e != nil {
		t.Fatal(e)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	process, e := pluginhost.Start(ctx, pluginhost.Spec{ID: "tangent.plugin.messaging", ExpectedID: "tangent.plugin.messaging", ExpectedVersion: "0.1.0-dev", Command: binary, Env: []string{"HOME=" + privateDir(t), "TANGENT_MESSAGING_CONFIG=" + path, "TANGENT_MCP_URL=" + mcpURL, "TETHER_TOKEN="}, Init: subprocess.InitParams{PluginDir: filepath.Dir(binary), DataDir: data, CacheDir: cache, Config: map[string]string{}, CapabilityContract: capability.ContractVersion, Incarnation: capability.RuntimeIdentity{HostInstance: "stage1-private-host", OwnerID: "tangent.plugin.messaging", OwnerGeneration: generation}, Grants: capability.GrantSet{}, HostInfo: subprocess.HostInfo{Version: "0.19.0", Protocol: 2}}, HandshakeTimeout: 10 * time.Second, UnloadTimeout: 5 * time.Second, ReapTimeout: 2 * time.Second})
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() {
		if e := process.Stop(context.Background()); e != nil {
			t.Error(e, process.Diagnostics())
		}
	})
	return process
}
func stopPlugin(t *testing.T, p *pluginhost.Process) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if e := p.Stop(ctx); e != nil {
		t.Fatal(e, p.Diagnostics())
	}
	info, ok := p.ExitInfo()
	if !ok || info.Code != 0 {
		t.Fatal("natural plugin reap", info, p.Diagnostics())
	}
}
