// Command tangent-plugin-messaging consumes committed channel publications.
// Configuration belongs to the plugin, and all persistence uses Init.DataDir.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"

	pipeline "github.com/hollis-labs/libs/message-pipeline"
	"github.com/hollis-labs/libs/plugin-mcp/plugin-sdk/capability"
	"github.com/hollis-labs/libs/plugin-mcp/plugin-sdk/subprocess"
	"github.com/hollis-labs/tangent-plugins/messaging/internal/messaging"
	"github.com/hollis-labs/tangent-plugins/messaging/internal/reply"
	"github.com/hollis-labs/tangent-plugins/messaging/internal/replyprojection"
	"github.com/hollis-labs/tangent-plugins/messaging/internal/tangentsink"
	"github.com/hollis-labs/tangent/pkg/plugin/hostclient"
)

const (
	pluginID      = "tangent.plugin.messaging"
	pluginVersion = "0.1.0-dev"
	configEnv     = "TANGENT_MESSAGING_CONFIG"
)

type served struct {
	mu          sync.Mutex
	config      messaging.Config
	ledger      *messaging.Ledger
	client      *hostclient.Client
	source      messaging.ChannelSource
	specs       []pipeline.StageSpec
	closeSource func()
	closeStages func()
	consumer    *messaging.Consumer
	replies     *messaging.ReplyStore
	dispatcher  reply.Dispatcher
	replyWorker *messaging.ReplyWorker
	replyHTTP   *replyprojection.Handler
}

func readConfiguration(path string) (messaging.Config, error) {
	if !filepath.IsAbs(path) {
		return messaging.Config{}, errors.New("messaging: explicit_configuration_path_required")
	}
	root, err := os.OpenRoot(filepath.Dir(path))
	if err != nil {
		return messaging.Config{}, errors.New("messaging: configuration_unavailable")
	}
	file, err := root.Open(filepath.Base(path))
	if err != nil {
		return messaging.Config{}, errors.Join(errors.New("messaging: configuration_unavailable"), root.Close())
	}
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return messaging.Config{}, errors.Join(errors.New("messaging: configuration_unavailable"), file.Close(), root.Close())
	}
	config, readErr := messaging.ReadConfig(file)
	return config, errors.Join(readErr, file.Close(), root.Close())
}

func (s *served) Init(ctx context.Context, params subprocess.InitParams) (subprocess.InitResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ledger != nil {
		return subprocess.InitResult{}, errors.New("messaging: already_initialized")
	}
	config, err := readConfiguration(os.Getenv(configEnv))
	if err != nil {
		return subprocess.InitResult{}, err
	}
	ledger, err := messaging.OpenLedger(ctx, params.DataDir)
	if err != nil {
		return subprocess.InitResult{}, err
	}
	source, closeSource, err := messaging.NewSource(config, os.Getenv("TETHER_TOKEN"))
	if err != nil {
		return subprocess.InitResult{}, errors.Join(err, ledger.Close())
	}
	specs, closeStages, err := messaging.BuildStages(config, os.Getenv("TETHER_TOKEN"))
	if err != nil {
		closeSource()
		return subprocess.InitResult{}, errors.Join(err, ledger.Close())
	}
	client, err := hostclient.New()
	if err != nil {
		closeSource()
		closeStages()
		return subprocess.InitResult{}, errors.Join(errors.New("messaging: host_endpoint_required"), ledger.Close())
	}
	sources := make([]messaging.Source, 0, len(config.Channels))
	for _, channel := range config.Channels {
		sources = append(sources, messaging.Source{EndpointRef: config.EndpointRef, Channel: channel})
	}
	replies, err := messaging.NewReplyStore(ctx, ledger, sources...)
	if err != nil {
		closeSource()
		closeStages()
		return subprocess.InitResult{}, errors.Join(err, client.Close(), ledger.Close())
	}
	s.config, s.ledger, s.source, s.specs = config, ledger, source, specs
	s.replies, s.dispatcher = replies, source
	s.client, s.closeSource, s.closeStages = client, closeSource, closeStages
	return subprocess.InitResult{ID: pluginID, Name: "Messaging Consumer", Version: pluginVersion, Description: "Durable publication intake with bounded stateless stage processing", Protocol: subprocess.ProtocolVersion, CapabilityContract: capability.ContractVersion}, nil
}

func (s *served) Load(ctx context.Context) (subprocess.LoadResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ledger == nil || s.consumer != nil {
		return subprocess.LoadResult{}, errors.New("messaging: load_state_refused")
	}
	processor, err := messaging.NewProcessor(s.specs, s.ledger)
	if err != nil {
		return subprocess.LoadResult{}, err
	}
	consumer, err := messaging.NewConsumer(s.config, s.ledger, s.source, processor, tangentsink.New(s.client))
	if err != nil {
		return subprocess.LoadResult{}, err
	}
	host := reply.ToolHost{Caller: s.client}
	controller, err := reply.New(s.replies, s.dispatcher, host, s.config.CallerURN, s.config.RequestTimeout())
	if err != nil {
		return subprocess.LoadResult{}, err
	}
	worker, err := messaging.NewReplyWorker(s.config, s.replies, host, controller)
	if err != nil {
		return subprocess.LoadResult{}, err
	}
	if err = consumer.Load(ctx); err != nil {
		return subprocess.LoadResult{}, err
	}
	s.consumer = consumer
	if err = worker.Load(ctx); err != nil {
		// Consumer ownership is retained for the host's eventual Unload.
		return subprocess.LoadResult{}, err
	}
	s.replyWorker = worker
	s.replyHTTP = &replyprojection.Handler{Reader: replyprojection.Reader{Mappings: s.replies, Store: s.replies,
		Dispatcher: s.dispatcher, Timeout: s.config.RequestTimeout()}, Controller: controller}
	return subprocess.LoadResult{}, nil
}

func (s *served) Unload(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.replyWorker != nil {
		if err := s.replyWorker.Unload(ctx); err != nil {
			return err
		}
	}
	if s.consumer != nil {
		if err := s.consumer.Unload(ctx); err != nil {
			return err
		}
	}
	// A failed join retains these ports and the ledger lock. The process owner
	// can contain the child; a cleanup deadline never claims natural cleanup.
	var failures []error
	if s.client != nil {
		failures = append(failures, s.client.Close())
		s.client = nil
	}
	if s.closeStages != nil {
		s.closeStages()
		s.closeStages = nil
	}
	if s.closeSource != nil {
		s.closeSource()
		s.closeSource = nil
	}
	if s.ledger != nil {
		failures = append(failures, s.ledger.Close())
		s.ledger = nil
	}
	s.consumer = nil
	s.replyWorker, s.replyHTTP, s.replies, s.dispatcher = nil, nil, nil, nil
	return errors.Join(failures...)
}

func (s *served) Health(ctx context.Context) (subprocess.HealthStatus, error) {
	if err := ctx.Err(); err != nil {
		return subprocess.HealthStatus{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.consumer == nil {
		return subprocess.HealthStatus{OK: false, Message: "not_loaded"}, nil
	}
	ok, code := s.consumer.Health()
	if ok && s.replyWorker != nil {
		ok, code = s.replyWorker.Health()
	}
	return subprocess.HealthStatus{OK: ok, Message: code}, nil
}

func (s *served) HTTPHandle(ctx context.Context, request subprocess.HTTPRequest) (subprocess.HTTPResponse, error) {
	s.mu.Lock()
	worker, handler := s.replyWorker, s.replyHTTP
	s.mu.Unlock()
	if worker == nil || handler == nil {
		return subprocess.HTTPResponse{Status: 503, Body: []byte(`{"code":"not_loaded"}`)}, nil
	}
	var response subprocess.HTTPResponse
	err := worker.Do(ctx, func(owned context.Context) error {
		var callErr error
		response, callErr = handler.HTTPHandle(owned, request)
		return callErr
	})
	return response, err
}

var (
	_ subprocess.Plugin        = (*served)(nil)
	_ subprocess.HealthChecker = (*served)(nil)
	_ subprocess.HTTPHandler   = (*served)(nil)
)

func run(args []string, out io.Writer) error {
	if len(args) == 1 && args[0] == "--manifest" {
		return emitManifest(out)
	}
	if len(args) != 0 {
		return errors.New("messaging: unexpected_arguments")
	}
	return subprocess.Serve(&served{})
}
func main() {
	if err := run(os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
