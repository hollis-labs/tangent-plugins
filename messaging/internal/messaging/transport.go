package messaging

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"time"

	tether "github.com/hollis-labs/go-tether-client"
)

const maxHistoryBytes = 16 * 1024 * 1024

// NewSource uses the published client, with explicit credentials (including an
// explicit empty choice). It never discovers credentials from an ambient home.
// The owner closes idle connections only after subscriptions have joined.
func NewSource(config Config, token string) (*tether.Client, func(), error) {
	if err := config.Validate(); err != nil {
		return nil, nil, err
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.ResponseHeaderTimeout = config.RequestTimeout()
	transport.IdleConnTimeout = time.Minute
	if strings.HasPrefix(config.TetherAddress, "unix:") {
		socket := strings.TrimPrefix(config.TetherAddress, "unix:")
		transport.Proxy = nil
		transport.DialContext = func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{Timeout: config.RequestTimeout()}).DialContext(ctx, "unix", socket)
		}
	}
	remote := !strings.HasPrefix(config.TetherAddress, "unix:")
	client := &http.Client{Transport: historyTransport{base: transport, remote: remote}, Timeout: config.RequestTimeout(), CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }}
	source, err := tether.New(config.TetherAddress, tether.WithSelfURN(config.CallerURN), tether.WithToken(token), tether.WithHTTPClient(client))
	if err != nil {
		transport.CloseIdleConnections()
		return nil, nil, errors.New("messaging: source_configuration_refused")
	}
	return source, transport.CloseIdleConnections, nil
}

// protocolHeader is the Tether environment protocol a remote listener requires
// on every request; it answers 409 protocol_mismatch without it. The published
// client sends none, and a local Unix socket does not ask for it.
const (
	protocolHeader = "Tether-Protocol"
	protocolValue  = "1"
)

type historyTransport struct {
	base   http.RoundTripper
	remote bool
}

func (t historyTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	if t.remote && request.Header.Get(protocolHeader) == "" {
		request = request.Clone(request.Context())
		request.Header.Set(protocolHeader, protocolValue)
	}
	response, err := t.base.RoundTrip(request)
	if err != nil {
		return nil, err
	}
	if strings.HasSuffix(request.URL.Path, "/messages") {
		response.Body = &boundedBody{ReadCloser: response.Body, remaining: maxHistoryBytes}
	}
	return response, nil
}

type boundedBody struct {
	io.ReadCloser
	remaining int64
}

func (b *boundedBody) Read(buffer []byte) (int, error) {
	if b.remaining <= 0 {
		return 0, errors.New("messaging: history_body_limit")
	}
	if int64(len(buffer)) > b.remaining {
		buffer = buffer[:b.remaining]
	}
	n, err := b.ReadCloser.Read(buffer)
	b.remaining -= int64(n)
	return n, err
}
