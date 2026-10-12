package messaging

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	tether "github.com/hollis-labs/go-tether-client"
)

func TestSourceUsesExplicitCallerAndCredentialChoiceWithoutRedirect(t *testing.T) {
	var targetCalls int
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { targetCalls++; w.WriteHeader(http.StatusOK) }))
	defer target.Close()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("as") != "msg://service/local/owner-inbox" || r.Header.Get("Authorization") != "Bearer synthetic-owned-token" {
			t.Error("explicit caller/token not preserved")
		}
		http.Redirect(w, r, target.URL, http.StatusFound)
	}))
	defer server.Close()
	config := testConfig()
	config.TetherAddress = server.URL
	client, closeTransport, err := NewSource(config, "synthetic-owned-token")
	if err != nil {
		t.Fatal(err)
	}
	defer closeTransport()
	if _, err = client.ChannelMessages(context.Background(), "owner-inbox", tether.ChannelMessagesOptions{}); err == nil {
		t.Fatal("redirect accepted")
	}
	if targetCalls != 0 {
		t.Fatal("source credential redirected")
	}
}

func TestHistoryBodyBoundDoesNotTruncateIntoSuccess(t *testing.T) {
	body := &boundedBody{ReadCloser: io.NopCloser(strings.NewReader("larger-than-limit")), remaining: 4}
	_, err := io.ReadAll(body)
	if err == nil {
		t.Fatal("truncated history treated as EOF/success")
	}
}

// A remote Tether listener answers 409 protocol_mismatch to any request without
// the Tether-Protocol header, and the published client sends none.
func TestRemoteSourceSendsTetherProtocolHeader(t *testing.T) {
	var got string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Get("Tether-Protocol")
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"name":"owner-inbox","address":"msg://service/local/channel/owner-inbox","messages":[]}`)
	}))
	defer server.Close()
	config := testConfig()
	config.TetherAddress = server.URL
	client, closeTransport, err := NewSource(config, "synthetic-owned-token")
	if err != nil {
		t.Fatal(err)
	}
	defer closeTransport()
	if _, err = client.ChannelMessages(context.Background(), "owner-inbox", tether.ChannelMessagesOptions{}); err != nil {
		t.Fatal(err)
	}
	if got != "1" {
		t.Fatalf("Tether-Protocol = %q, want 1", got)
	}
}
