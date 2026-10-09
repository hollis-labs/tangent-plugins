//go:build !windows

package summarizer

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestExplicitUnixSocketAndOwnerTransportSnapshot(t *testing.T) {
	// Unix socket pathname limits are shorter than the private test TMPDIR.
	dir, err := os.MkdirTemp("/tmp", "summary-unix-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	socket := filepath.Join(dir, "gateway.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/ai/chat" || r.Host != "tether" {
			t.Error("wrong Unix gateway route")
		}
		_, _ = w.Write(encoded(t, `{"summary":"Unix fixture summary."}`))
	}))
	_ = server.Listener.Close()
	server.Listener = listener
	server.Start()
	defer server.Close()
	transport := http.DefaultTransport.(*http.Transport).Clone()
	owner := &http.Client{Transport: transport}
	stage, err := New(Config{EndpointURL: "unix:" + socket, CallerID: "caller"}, owner)
	if err != nil {
		t.Fatal(err)
	}
	if stage.client.Transport == transport || transport.Proxy == nil {
		t.Fatal("mutated owner transport")
	}
	result, err := stage.Run(context.Background(), input())
	if err != nil || len(result.Summaries) != 1 {
		t.Fatal(result, err)
	}
	other, err := New(Config{EndpointURL: "unix:" + socket + ".other", CallerID: "caller"}, nil)
	if err != nil || other.ConfigDigest() == stage.ConfigDigest() {
		t.Fatal("socket identity not bound", err)
	}
	for _, address := range []string{"unix:relative", "unix:/", "unix://host/path"} {
		if _, err = New(Config{EndpointURL: address, CallerID: "caller"}, nil); err == nil {
			t.Fatal("accepted invalid Unix endpoint")
		}
	}
	stage.client.CloseIdleConnections()
}
