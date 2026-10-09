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
	client := &http.Client{Transport: historyTransport{base: transport}, Timeout: config.RequestTimeout(), CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }}
	source, err := tether.New(config.TetherAddress, tether.WithSelfURN(config.CallerURN), tether.WithToken(token), tether.WithHTTPClient(client))
	if err != nil {
		transport.CloseIdleConnections()
		return nil, nil, errors.New("messaging: source_configuration_refused")
	}
	return source, transport.CloseIdleConnections, nil
}

type historyTransport struct{ base http.RoundTripper }

func (t historyTransport) RoundTrip(request *http.Request) (*http.Response, error) {
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
