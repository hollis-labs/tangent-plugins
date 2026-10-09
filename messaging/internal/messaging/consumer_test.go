package messaging

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	tether "github.com/hollis-labs/go-tether-client"
	pipeline "github.com/hollis-labs/libs/message-pipeline"
)

type fakeProcessor struct{ calls int }

func (p *fakeProcessor) Run(_ context.Context, m pipeline.Message, _ pipeline.State) (pipeline.Settlement, error) {
	p.calls++
	return pipeline.Settlement{Message: m, Disposition: pipeline.Pass, State: pipeline.State{Annotations: []pipeline.Annotation{{SchemaVersion: 1, StageID: "summary", StageVersion: "1", Kind: "summary", Summary: pipeline.Summary{Text: "saved summary"}}}}}, nil
}

type fakeSink struct {
	calls [][]byte
	fail  bool
}

func (s *fakeSink) Prepare(p Publication, state pipeline.State) ([]byte, error) {
	return json.Marshal(struct {
		Key, Original string
		State         pipeline.State
	}{p.Key(), p.Original, state})
}
func (s *fakeSink) Deliver(_ context.Context, payload []byte) (SinkReceipt, error) {
	s.calls = append(s.calls, bytes.Clone(payload))
	if s.fail {
		s.fail = false
		return SinkReceipt{}, errors.New("owned ambiguous commit")
	}
	var request struct{ Key string }
	if err := json.Unmarshal(payload, &request); err != nil {
		return SinkReceipt{}, err
	}
	return SinkReceipt{ItemID: "same-sink-item", IdempotencyKey: request.Key}, nil
}

type fakeSource struct {
	mu           sync.Mutex
	pages        []tether.ChannelMessagesResponse
	historySince []int64
	subscribed   chan int64
	joined       chan struct{}
	events       chan tether.ChannelMessage
	streamErrors chan error
}

func (s *fakeSource) ChannelMessages(_ context.Context, _ string, options tether.ChannelMessagesOptions) (tether.ChannelMessagesResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.historySince = append(s.historySince, options.Since)
	if len(s.pages) == 0 {
		return tether.ChannelMessagesResponse{Channel: tether.Channel{Name: "owner-inbox", Address: "msg://service/local/channel/owner-inbox"}}, nil
	}
	page := s.pages[0]
	s.pages = s.pages[1:]
	return page, nil
}
func (s *fakeSource) SubscribeChannel(ctx context.Context, _ string, since *int64) (<-chan tether.ChannelMessage, <-chan error, error) {
	if since == nil {
		return nil, nil, errors.New("lost replay cursor")
	}
	s.subscribed <- *since
	go func() { <-ctx.Done(); close(s.events); close(s.streamErrors); close(s.joined) }()
	return s.events, s.streamErrors, nil
}
func consumerConfig() Config {
	return Config{SchemaVersion: 1, EndpointRef: "tether-owner", TetherAddress: "unix:/tmp/owned-fixture.sock", CallerURN: "msg://agent/local/test", Channels: []string{"owner-inbox"}, RequestTimeoutMS: 1000, ReconnectMinMS: 5, ReconnectMaxMS: 10, HistoryLimit: 10, Stages: []StageConfig{{ID: "summarize", EndpointURL: "http://fixture.test/ai/chat", CallerID: "messaging-test"}}}
}
func sourceFixture() *fakeSource {
	return &fakeSource{subscribed: make(chan int64, 1), joined: make(chan struct{}), events: make(chan tether.ChannelMessage), streamErrors: make(chan error)}
}
func newTestConsumer(t *testing.T, l *Ledger, p Processor, sink Sink, source *fakeSource) *Consumer {
	t.Helper()
	c, err := NewConsumer(consumerConfig(), l, source, p, sink)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestConsumerAmbiguousDeliveryReusesSavedStageAndExactRequest(t *testing.T) {
	l, _ := openTestLedger(t)
	p := &fakeProcessor{}
	sink := &fakeSink{fail: true}
	c := newTestConsumer(t, l, p, sink, sourceFixture())
	message := routedMessage()
	if err := c.process(context.Background(), testSource(), message); err == nil {
		t.Fatal("ambiguous receipt settled")
	}
	if _, found, err := l.Cursor(context.Background(), testSource()); err != nil || found {
		t.Fatal("ambiguous cursor advanced", err)
	}
	if err := c.process(context.Background(), testSource(), message); err != nil {
		t.Fatal(err)
	}
	if p.calls != 1 || len(sink.calls) != 2 || !bytes.Equal(sink.calls[0], sink.calls[1]) {
		t.Fatal("replay regenerated provider/request")
	}
}

func TestConsumerSinkCommitBeforeLocalRollbackReplaysEarnedItem(t *testing.T) {
	l, _ := openTestLedger(t)
	p := &fakeProcessor{}
	sink := &fakeSink{}
	c := newTestConsumer(t, l, p, sink, sourceFixture())
	if _, err := l.db.Exec(`CREATE TRIGGER fail_cursor BEFORE INSERT ON cursors BEGIN SELECT RAISE(ABORT,'owned crash boundary'); END`); err != nil {
		t.Fatal(err)
	}
	if err := c.process(context.Background(), testSource(), routedMessage()); err == nil {
		t.Fatal("local rollback ignored")
	}
	if _, err := l.db.Exec(`DROP TRIGGER fail_cursor`); err != nil {
		t.Fatal(err)
	}
	if err := c.process(context.Background(), testSource(), routedMessage()); err != nil {
		t.Fatal(err)
	}
	if p.calls != 1 || len(sink.calls) != 2 || !bytes.Equal(sink.calls[0], sink.calls[1]) {
		t.Fatal("committed sink crash generated another request")
	}
}

func TestConsumerAdmissionRefusalBlocksLaterSettlementWithoutCallingStage(t *testing.T) {
	l, _ := openTestLedger(t)
	p := &fakeProcessor{}
	sink := &fakeSink{}
	source := sourceFixture()
	bad := routedMessage()
	bad.Metadata["kind"] = "unknown"
	later := routedMessage()
	later.ID = "later"
	later.Seq = 99
	source.pages = []tether.ChannelMessagesResponse{{Channel: tether.Channel{Name: "owner-inbox", Address: "msg://service/local/channel/owner-inbox"}, Messages: []tether.ChannelMessage{bad, later}, NextSince: 1000}}
	c := newTestConsumer(t, l, p, sink, source)
	if err := c.history(context.Background(), testSource()); err == nil {
		t.Fatal("admission refusal skipped")
	}
	if p.calls != 0 || len(sink.calls) != 0 {
		t.Fatal("refused publication had effects")
	}
	var refusals int
	if err := l.db.QueryRow(`SELECT COUNT(*) FROM admission_refusals`).Scan(&refusals); err != nil || refusals != 1 {
		t.Fatal("refusal obligation not recorded", refusals, err)
	}
	if _, found, err := l.Cursor(context.Background(), testSource()); err != nil || found {
		t.Fatal("source hint advanced cursor", err)
	}
}

func TestConsumerMalformedAttributionRefusesBeforeStage(t *testing.T) {
	l, _ := openTestLedger(t)
	p := &fakeProcessor{}
	sink := &fakeSink{}
	source := sourceFixture()
	bad := routedMessage()
	bad.Metadata["confidence"] = "invented"
	later := routedMessage()
	later.ID = "later"
	later.Seq = 99
	source.pages = []tether.ChannelMessagesResponse{{Channel: tether.Channel{Name: "owner-inbox", Address: "msg://service/local/channel/owner-inbox"}, Messages: []tether.ChannelMessage{bad, later}, NextSince: 1000}}
	c := newTestConsumer(t, l, p, sink, source)
	if err := c.history(context.Background(), testSource()); err == nil {
		t.Fatal("admission refusal skipped")
	}
	if p.calls != 0 || len(sink.calls) != 0 {
		t.Fatal("refused publication had effects")
	}
	var refusals int
	if err := l.db.QueryRow(`SELECT COUNT(*) FROM admission_refusals`).Scan(&refusals); err != nil || refusals != 1 {
		t.Fatal("refusal obligation not recorded", refusals, err)
	}
	if _, found, err := l.Cursor(context.Background(), testSource()); err != nil || found {
		t.Fatal("source hint advanced cursor", err)
	}
}

func TestConsumerLoadLifetimeAndUnloadJoinOwnedStream(t *testing.T) {
	l, _ := openTestLedger(t)
	source := sourceFixture()
	c := newTestConsumer(t, l, &fakeProcessor{}, &fakeSink{}, source)
	load, cancel := context.WithCancel(context.Background())
	if err := c.Load(load); err != nil {
		t.Fatal(err)
	}
	cancel()
	select {
	case since := <-source.subscribed:
		if since != 0 {
			t.Fatal("absent cursor must replay from pointer zero")
		}
	case <-time.After(time.Second):
		t.Fatal("Load context killed incarnation")
	}
	select {
	case <-source.joined:
		t.Fatal("Load cancellation killed active stream")
	default:
	}
	unload, cancelUnload := context.WithTimeout(context.Background(), time.Second)
	defer cancelUnload()
	if err := c.Unload(unload); err != nil {
		t.Fatal(err)
	}
	select {
	case <-source.joined:
	default:
		t.Fatal("Unload returned before stream join")
	}
	if ok, code := c.Health(); ok || code != "not_loaded" {
		t.Fatal("unloaded health", ok, code)
	}
}

func TestOwnedProcessorShutdownJoinsStageBeforePortClosure(t *testing.T) {
	l, _ := openTestLedger(t)
	entered := make(chan struct{})
	release := make(chan struct{})
	returned := make(chan struct{})
	var calls atomic.Int32
	stage := pipeline.StageFunc(func(ctx context.Context, _ pipeline.Input) (pipeline.Result, error) {
		calls.Add(1)
		close(entered)
		<-ctx.Done()
		<-release
		close(returned)
		return pipeline.Result{}, ctx.Err()
	})
	p, err := NewProcessor([]pipeline.StageSpec{{ID: "summary", Version: "1", ConfigDigest: "config", InstructionDigest: "instruction", Timeout: time.Second, Stage: stage}}, l)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	runDone := make(chan struct{})
	go func() {
		_, _ = p.Run(ctx, pipeline.Message{Identity: pipeline.Identity{Source: "source", Message: "message"}, Original: "original"}, pipeline.State{})
		close(runDone)
	}()
	<-entered
	cancel()
	<-runDone
	deadline, end := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer end()
	if err := p.Join(deadline); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("join claimed unwinding stage had exited", err)
	}
	close(release)
	if err := p.Join(context.Background()); err != nil {
		t.Fatal(err)
	}
	select {
	case <-returned:
	default:
		t.Fatal("stage still owns ports")
	}
	if calls.Load() != 1 {
		t.Fatal("unexpected stage execution")
	}
}
