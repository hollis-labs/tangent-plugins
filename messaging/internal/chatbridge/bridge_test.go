package chatbridge

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/hollis-labs/libs/plugin-mcp/plugin-sdk/subprocess"
)

func fake(t *testing.T, handler http.HandlerFunc) *Bridge {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	b, err := New(server.URL, "secret-token", 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(b.Close)
	return b
}

func call(t *testing.T, b *Bridge, method, path, body string) (int, map[string]any) {
	t.Helper()
	resp, err := b.HTTPHandle(context.Background(), subprocess.HTTPRequest{Method: method, Path: path, Body: []byte(body)})
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	if err := json.Unmarshal(resp.Body, &out); err != nil {
		t.Fatal(err)
	}
	return resp.Status, out
}

func TestAgentsForwardsWithAuthAndProtocol(t *testing.T) {
	b := fake(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/agents" || r.Header.Get("Authorization") != "Bearer secret-token" || r.Header.Get("Tether-Protocol") != "1" {
			t.Errorf("bad request %s %v", r.URL.Path, r.Header)
		}
		io.WriteString(w, `{"agents":[{"launch_id":"pm","agent_id":"pm","display_name":"PM","channel":"c","session":null}]}`)
	})
	status, out := call(t, b, "GET", AgentsPath, "")
	agents, _ := out["agents"].([]any)
	if status != 200 || out["available"] != true || len(agents) != 1 {
		t.Fatalf("%d %v", status, out)
	}
}

func TestAgentsUnavailableStates(t *testing.T) {
	for code, reason := range map[int]string{404: "tether_endpoint_missing", 403: "tether_forbidden", 500: "tether_error"} {
		b := fake(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(code) })
		status, out := call(t, b, "GET", AgentsPath, "")
		if status != 200 || out["available"] != false || out["reason"] != reason {
			t.Errorf("%d: %d %v", code, status, out)
		}
	}
	b, _ := New("http://127.0.0.1:1", "", time.Second)
	if _, out := call(t, b, "GET", AgentsPath, ""); out["reason"] != "tether_unreachable" {
		t.Errorf("unreachable: %v", out)
	}
}

func TestSendMapsOutcomes(t *testing.T) {
	var got map[string]string
	status := 201
	body := `{"mode":"launch","session_id":"s1","replayed":false}`
	b := fake(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/agents/pm/send" || r.Method != "POST" {
			t.Errorf("bad request %s", r.URL.Path)
		}
		json.NewDecoder(r.Body).Decode(&got)
		w.WriteHeader(status)
		io.WriteString(w, body)
	})
	req := `{"launch_id":"pm","text":"hi","idempotency_key":"k"}`
	if s, out := call(t, b, "POST", SendPath, req); s != 200 || out["ok"] != true || out["mode"] != "launch" || got["text"] != "hi" || got["idempotency_key"] != "k" {
		t.Fatalf("%d %v %v", s, out, got)
	}
	status, body = 409, `{"error":{"code":"agent_working"}}`
	if s, out := call(t, b, "POST", SendPath, req); s != 409 || out["code"] != "agent_working" {
		t.Fatalf("%d %v", s, out)
	}
	status, body = 404, `{"error":{"code":"not_found"}}`
	if s, out := call(t, b, "POST", SendPath, req); s != 404 || out["code"] != "chat_unavailable" {
		t.Fatalf("%d %v", s, out)
	}
}

func TestSendRejectsBadInput(t *testing.T) {
	b := fake(t, func(http.ResponseWriter, *http.Request) { t.Error("must not reach tether") })
	for _, body := range []string{
		``, `{}`, `{"launch_id":"../x","text":"a","idempotency_key":"k"}`,
		`{"launch_id":"pm","text":" ","idempotency_key":"k"}`,
		`{"launch_id":"pm","text":"a"}`,
		`{"launch_id":"pm","text":"a","idempotency_key":"k","x":1}`,
	} {
		if s, _ := call(t, b, "POST", SendPath, body); s != 400 {
			t.Errorf("%q: %d", body, s)
		}
	}
}

func TestNewRejectsCredentialedAddress(t *testing.T) {
	if _, err := New("https://user:pw@host", "", time.Second); err == nil {
		t.Fatal("accepted credentials in address")
	}
}
