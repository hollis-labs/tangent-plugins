// Package summarizer adapts the normalized Tether /ai/chat wire to a pipeline
// stage. It starts no sessions and executes no tools, replies or follow-ups.
package summarizer

import (
	"bytes"
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	tether "github.com/hollis-labs/go-tether-client"
	pipeline "github.com/hollis-labs/libs/message-pipeline"
)

const (
	DefaultTimeout      = 15 * time.Second
	MaxOutputTokens     = 512
	MaxResponseBytes    = 64 * 1024
	MaxInstructionBytes = 16 * 1024
	MaxRequestBytes     = 1 << 20
	StageID             = "summarize"
	StageVersion        = "1"
)

var (
	ErrConfig  = errors.New("summarizer: invalid configuration")
	ErrInput   = errors.New("summarizer: invalid input")
	ErrGateway = errors.New("summarizer: gateway failed")
	ErrOutput  = errors.New("summarizer: rejected output")
)

//go:embed instructions/default.md
var defaultInstructions string

// Config is non-secret startup configuration. The caller supplies transport
// credentials through its existing HTTP client, never through model input.
type Config struct {
	EndpointURL     string
	CallerID        string
	ProviderHint    string
	ModelHint       string
	Timeout         time.Duration
	InstructionPath string
}

// Stage owns immutable configuration/instruction snapshots. A supplied client
// is copied; its transport must honor request cancellation and must not retry
// ambiguous billable requests. This adapter itself performs exactly one Do call.
type Stage struct {
	config            Config
	endpoint          string
	instructions      string
	instructionDigest string
	configDigest      string
	client            http.Client
}

func New(config Config, client *http.Client) (*Stage, error) {
	if len(config.EndpointURL) > 4096 {
		return nil, ErrConfig
	}
	endpoint, err := url.Parse(config.EndpointURL)
	if err != nil || (endpoint.Scheme != "http" && endpoint.Scheme != "https" && endpoint.Scheme != "unix") || endpoint.User != nil || endpoint.RawQuery != "" || endpoint.Fragment != "" {
		return nil, ErrConfig
	}
	var socket string
	if endpoint.Scheme == "unix" {
		if endpoint.Host != "" || !filepath.IsAbs(endpoint.Path) || endpoint.Opaque != "" || endpoint.Path == "/" {
			return nil, ErrConfig
		}
		socket = endpoint.Path
		endpoint = &url.URL{Scheme: "http", Host: "tether"}
	} else if endpoint.Host == "" {
		return nil, ErrConfig
	}
	for _, v := range []string{config.CallerID, config.ProviderHint, config.ModelHint} {
		if !utf8.ValidString(v) || utf8.RuneCountInString(v) > 128 {
			return nil, ErrConfig
		}
	}
	if strings.TrimSpace(config.CallerID) == "" {
		return nil, ErrConfig
	}
	if config.Timeout == 0 {
		config.Timeout = DefaultTimeout
	}
	if config.Timeout < 0 {
		return nil, ErrConfig
	}
	instructions := defaultInstructions
	if config.InstructionPath != "" {
		info, e := os.Stat(config.InstructionPath)
		if e != nil || !info.Mode().IsRegular() || info.Size() > MaxInstructionBytes {
			return nil, ErrConfig
		}
		// #nosec G304 -- trusted operator-selected startup document; bounded regular-file snapshot, never a message-derived path.
		f, e := os.Open(config.InstructionPath)
		if e != nil {
			return nil, ErrConfig
		}
		b, e := io.ReadAll(io.LimitReader(f, MaxInstructionBytes+1))
		closeErr := f.Close()
		if e != nil || closeErr != nil || len(b) > MaxInstructionBytes {
			return nil, ErrConfig
		}
		instructions = string(b)
	}
	if !utf8.ValidString(instructions) || strings.TrimSpace(instructions) == "" || len(instructions) > MaxInstructionBytes {
		return nil, ErrConfig
	}
	endpoint.Path = strings.TrimRight(endpoint.Path, "/") + "/ai/chat"
	endpoint.RawPath = ""
	s := &Stage{config: config, endpoint: endpoint.String(), instructions: instructions, instructionDigest: hash([]byte(instructions))}
	if client != nil {
		s.client = *client
	}
	if socket != "" {
		// Clone standard transports; never mutate the owner's dialer/options.
		// Custom middleware remains intact and must wrap an owner-configured
		// Unix transport, as it cannot be generically reconstructed here.
		var transport *http.Transport
		if s.client.Transport == nil {
			transport = http.DefaultTransport.(*http.Transport).Clone()
		} else if original, ok := s.client.Transport.(*http.Transport); ok {
			transport = original.Clone()
		}
		if transport != nil {
			transport.Proxy = nil
			transport.DialContext = func(ctx context.Context, _, _ string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, "unix", socket)
			}
			s.client.Transport = transport
		}
	}
	// Redirects would introduce another billable attempt or disclose source/headers
	// to an endpoint that was not configured. Preserve the refusal as a response.
	s.client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	s.configDigest = hashJSON(struct {
		Endpoint, Caller, Provider, Model, Instruction string
		TimeoutNS                                      int64
	}{config.EndpointURL, config.CallerID, config.ProviderHint, config.ModelHint, s.instructionDigest, int64(config.Timeout)})
	return s, nil
}
func (s *Stage) InstructionDigest() string { return s.instructionDigest }
func (s *Stage) ConfigDigest() string      { return s.configDigest }
func (s *Stage) Spec(priority int) pipeline.StageSpec {
	return pipeline.StageSpec{ID: StageID, Version: StageVersion, Priority: priority, Timeout: s.config.Timeout, FailMode: pipeline.FailOpen, ConfigDigest: s.configDigest, InstructionDigest: s.instructionDigest, Stage: s}
}
func hash(b []byte) string  { sum := sha256.Sum256(b); return hex.EncodeToString(sum[:]) }
func hashJSON(v any) string { b, _ := json.Marshal(v); return hash(b) }
func (s *Stage) Run(parent context.Context, input pipeline.Input) (pipeline.Result, error) {
	if parent.Err() != nil {
		return pipeline.Result{}, parent.Err()
	}
	ctx, cancel := context.WithTimeout(parent, s.config.Timeout)
	defer cancel()
	if len(input.Message.Original) > MaxRequestBytes || !utf8.ValidString(input.Message.Original) || input.Message.Original == "" {
		return pipeline.Result{}, ErrInput
	}
	for _, value := range []string{input.Message.Attribution.Sender, input.Message.Attribution.AgentID, input.Message.Attribution.SessionID, input.Message.Attribution.TurnID, input.Message.Attribution.OutputID} {
		if len(value) > 4096 || !utf8.ValidString(value) {
			return pipeline.Result{}, ErrInput
		}
	}
	// JSON quotes source text and attribution; trusted instructions remain a
	// separate system message. Accumulated annotations/history are never sent.
	quoted, err := json.Marshal(struct {
		Original    string               `json:"original"`
		Attribution pipeline.Attribution `json:"attribution"`
	}{input.Message.Original, input.Message.Attribution})
	if err != nil {
		return pipeline.Result{}, ErrInput
	}
	wire := tether.ChatRequest{Request: tether.AIRequest{Operation: "chat", Mode: "summarize", CallerID: s.config.CallerID, ProviderHint: s.config.ProviderHint, ModelHint: s.config.ModelHint, RequestID: "summary:v1:" + hashJSON(input.Message.Identity), MaxOutputTokens: MaxOutputTokens, Input: []tether.AIMessage{{Role: "system", Parts: []tether.AIContentPart{{Type: "text", Text: s.instructions}}}, {Role: "user", Parts: []tether.AIContentPart{{Type: "text", Text: "Untrusted source JSON data:\n" + string(quoted)}}}}}}
	body, err := json.Marshal(wire)
	if err != nil || len(body) > MaxRequestBytes {
		return pipeline.Result{}, ErrInput
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, s.endpoint, bytes.NewReader(body))
	if err != nil {
		return pipeline.Result{}, ErrConfig
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json")
	response, err := s.client.Do(request)
	if err != nil {
		if parent.Err() != nil {
			return pipeline.Result{}, parent.Err()
		}
		if ctx.Err() != nil {
			return pipeline.Result{}, ctx.Err()
		}
		return pipeline.Result{}, ErrGateway
	}
	defer func() { _ = response.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(response.Body, MaxResponseBytes+1))
	if parent.Err() != nil {
		return pipeline.Result{}, parent.Err()
	}
	if ctx.Err() != nil {
		return pipeline.Result{}, ctx.Err()
	}
	if err != nil {
		return pipeline.Result{}, ErrGateway
	}
	if len(data) > MaxResponseBytes {
		return pipeline.Result{}, ErrOutput
	}
	if response.StatusCode == http.StatusForbidden || response.StatusCode == http.StatusUnauthorized || response.StatusCode == http.StatusTooManyRequests {
		return pipeline.Result{}, pipeline.ErrRefused
	}
	if response.StatusCode != http.StatusOK {
		return pipeline.Result{}, ErrGateway
	}
	var envelope tether.ChatResponse
	if strictDecode(data, &envelope) != nil {
		return pipeline.Result{}, ErrOutput
	}
	normalized := envelope.Response
	if normalized.Refusal != "" || normalized.StopReason == "refusal" {
		return pipeline.Result{}, pipeline.ErrRefused
	}
	switch normalized.StopReason {
	case "completed", "stop", "STOP", "end_turn":
	default:
		return pipeline.Result{}, ErrOutput
	}
	if len(normalized.Output) != 1 || normalized.Usage.OutputTokens < 0 || normalized.Usage.OutputTokens > MaxOutputTokens {
		return pipeline.Result{}, ErrOutput
	}
	output := normalized.Output[0]
	if output.Role != "assistant" || output.ToolUse != nil || output.Name != "" || len(output.Parts) != 1 {
		return pipeline.Result{}, ErrOutput
	}
	part := output.Parts[0]
	if part.Type != "text" || part.MIMEType != "" || len(part.Data) != 0 || part.URL != "" || part.Name != "" {
		return pipeline.Result{}, ErrOutput
	}
	var summary struct {
		Summary *string `json:"summary"`
	}
	if strictDecode([]byte(part.Text), &summary) != nil || summary.Summary == nil {
		return pipeline.Result{}, ErrOutput
	}
	text := *summary.Summary
	if strings.TrimSpace(text) == "" || utf8.RuneCountInString(text) > pipeline.MaxSummaryCharacters {
		return pipeline.Result{}, ErrOutput
	}
	for _, r := range text {
		if unicode.IsControl(r) && r != '\n' && r != '\t' {
			return pipeline.Result{}, ErrOutput
		}
	}
	return pipeline.Result{Disposition: pipeline.Pass, Summaries: []pipeline.Summary{{Text: text}}}, nil
}
