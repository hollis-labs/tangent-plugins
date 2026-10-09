package messaging

import (
	"context"
	"net"
	"net/http"
	"strings"
	"time"

	pipeline "github.com/hollis-labs/libs/message-pipeline"
	"github.com/hollis-labs/tangent-plugins/messaging/internal/summarizer"
)

// BuildStages snapshots instructions/configuration once per incarnation. The
// returned cleanup is used only after all stage executions have joined.
func BuildStages(config Config, token string) ([]pipeline.StageSpec, func(), error) {
	if err := config.Validate(); err != nil {
		return nil, nil, err
	}
	configured := config.Stages[0]
	budget := time.Duration(configured.TimeoutMS) * time.Millisecond
	if budget == 0 {
		budget = summarizer.DefaultTimeout
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.ResponseHeaderTimeout = budget
	if strings.HasPrefix(configured.EndpointURL, "unix:") {
		socket := strings.TrimPrefix(configured.EndpointURL, "unix:")
		transport.Proxy = nil
		transport.DialContext = func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{Timeout: budget}).DialContext(ctx, "unix", socket)
		}
	}
	client := &http.Client{Transport: explicitAuthTransport{base: transport, token: token}, Timeout: budget}
	stage, err := summarizer.New(summarizer.Config{EndpointURL: configured.EndpointURL, CallerID: configured.CallerID, ProviderHint: configured.ProviderHint, ModelHint: configured.ModelHint, Timeout: budget, InstructionPath: configured.InstructionPath}, client)
	if err != nil {
		transport.CloseIdleConnections()
		return nil, nil, err
	}
	return []pipeline.StageSpec{stage.Spec(configured.Priority)}, func() { stage.Close(); transport.CloseIdleConnections() }, nil
}

// Only an explicitly supplied token is used. The input request remains owned
// by the summarizer; middleware does not mutate it or retry billable work.
type explicitAuthTransport struct {
	base  http.RoundTripper
	token string
}

func (t explicitAuthTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	owned := request.Clone(request.Context())
	if t.token != "" {
		owned.Header.Set("Authorization", "Bearer "+t.token)
	}
	return t.base.RoundTrip(owned)
}

func (t explicitAuthTransport) CloseIdleConnections() {
	if owner, ok := t.base.(interface{ CloseIdleConnections() }); ok {
		owner.CloseIdleConnections()
	}
}
