package messaging

import (
	"context"
	"errors"
	"sync"
	"time"

	tether "github.com/hollis-labs/go-tether-client"
	pipeline "github.com/hollis-labs/libs/message-pipeline"
)

// ChannelSource is the public durable-history and owned-stream boundary.
type ChannelSource interface {
	ChannelMessages(context.Context, string, tether.ChannelMessagesOptions) (tether.ChannelMessagesResponse, error)
	SubscribeChannel(context.Context, string, *int64) (<-chan tether.ChannelMessage, <-chan error, error)
}

type Processor interface {
	Run(context.Context, pipeline.Message, pipeline.State) (pipeline.Settlement, error)
}

// Sink prepares without effects, then delivers only the saved immutable bytes.
// Delivery errors (including ambiguous commits) leave the publication pending.
type Sink interface {
	Prepare(Publication, pipeline.State) ([]byte, error)
	Deliver(context.Context, []byte) (SinkReceipt, error)
}

// Consumer owns workers for one loaded incarnation. The finite Load call is
// not their parent. Unload cancels their independent lifetime and joins them.
type Consumer struct {
	config    Config
	ledger    *Ledger
	source    ChannelSource
	processor Processor
	sink      Sink
	mu        sync.Mutex
	cancel    context.CancelFunc
	done      chan struct{}
	status    map[string]string
}

func NewConsumer(config Config, ledger *Ledger, source ChannelSource, processor Processor, sink Sink) (*Consumer, error) {
	if err := config.Validate(); err != nil {
		return nil, err
	}
	if ledger == nil || source == nil || processor == nil || sink == nil {
		return nil, errors.New("messaging: missing_consumer_port")
	}
	config.Channels = append([]string(nil), config.Channels...)
	config.Stages = append([]StageConfig(nil), config.Stages...)
	return &Consumer{config: config, ledger: ledger, source: source, processor: processor, sink: sink, status: make(map[string]string)}, nil
}

func (c *Consumer) Load(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.cancel != nil {
		return errors.New("messaging: already_loaded")
	}
	lifetime, cancel := context.WithCancel(context.Background())
	c.cancel, c.done = cancel, make(chan struct{})
	var workers sync.WaitGroup
	for _, channel := range c.config.Channels {
		workers.Add(1)
		c.status[channel] = "connecting"
		go func() { defer workers.Done(); c.runChannel(lifetime, channel) }()
	}
	go func(done chan struct{}) { workers.Wait(); close(done) }(c.done)
	return nil
}

func (c *Consumer) Unload(ctx context.Context) error {
	c.mu.Lock()
	cancel, done := c.cancel, c.done
	if cancel == nil {
		c.mu.Unlock()
		return nil
	}
	cancel()
	c.mu.Unlock()
	select {
	case <-done:
		if owner, ok := c.processor.(interface{ Join(context.Context) error }); ok {
			if err := owner.Join(ctx); err != nil {
				return err
			}
		}
		c.mu.Lock()
		if c.done == done {
			c.cancel = nil
		}
		c.mu.Unlock()
		return nil
	case <-ctx.Done():
		// The owner must retain the ledger/ports until the outstanding join
		// completes; a deadline never claims workers have gone away.
		return ctx.Err()
	}
}

// Health reports closed codes only, never message bodies or upstream errors.
func (c *Consumer) Health() (bool, string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.cancel == nil {
		return false, "not_loaded"
	}
	for _, channel := range c.config.Channels {
		if code := c.status[channel]; code != "watching" {
			return false, code
		}
	}
	return true, "watching"
}

func (c *Consumer) setStatus(channel, code string) {
	c.mu.Lock()
	c.status[channel] = code
	c.mu.Unlock()
}

func (c *Consumer) runChannel(ctx context.Context, channel string) {
	source := Source{EndpointRef: c.config.EndpointRef, Channel: channel}
	backoff := time.Duration(c.config.ReconnectMinMS) * time.Millisecond
	maximum := time.Duration(c.config.ReconnectMaxMS) * time.Millisecond
	for ctx.Err() == nil {
		if err := c.history(ctx, source); err == nil {
			backoff = time.Duration(c.config.ReconnectMinMS) * time.Millisecond
			c.watch(ctx, source)
		}
		if ctx.Err() != nil {
			return
		}
		timer := time.NewTimer(backoff)
		select {
		case <-timer.C:
		case <-ctx.Done():
			timer.Stop()
			return
		}
		if backoff < maximum/2 {
			backoff *= 2
		} else {
			backoff = maximum
		}
	}
}

func (c *Consumer) history(ctx context.Context, source Source) error {
	for {
		cursor, _, err := c.ledger.Cursor(ctx, source)
		if err != nil {
			c.setStatus(source.Channel, "ledger_pending")
			return err
		}
		request, cancel := context.WithTimeout(ctx, c.config.RequestTimeout())
		page, err := c.source.ChannelMessages(request, source.Channel, tether.ChannelMessagesOptions{Since: cursor, Limit: c.config.HistoryLimit})
		cancel()
		if err != nil {
			c.setStatus(source.Channel, "source_unavailable")
			return err
		}
		if page.Name != source.Channel || page.Address != "msg://service/local/channel/"+source.Channel || len(page.Messages) > c.config.HistoryLimit {
			c.setStatus(source.Channel, "source_contract_refused")
			return errors.New("messaging: source_contract_refused")
		}
		previous := cursor
		for _, message := range page.Messages {
			if message.Seq <= previous {
				c.setStatus(source.Channel, "source_contract_refused")
				return errors.New("messaging: unordered_history")
			}
			if err = c.process(ctx, source, message); err != nil {
				return err
			}
			previous = message.Seq
		}
		// NextSince is an upstream hint, never a committed cursor.
		if len(page.Messages) < c.config.HistoryLimit {
			return nil
		}
	}
}

func (c *Consumer) watch(ctx context.Context, source Source) {
	cursor, _, err := c.ledger.Cursor(ctx, source)
	if err != nil {
		c.setStatus(source.Channel, "ledger_pending")
		return
	}
	streamCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	// Bound stream establishment while allowing the established stream to
	// outlive that handshake budget under the loaded-incarnation lifetime.
	startTimer := time.AfterFunc(c.config.RequestTimeout(), cancel)
	events, errs, err := c.source.SubscribeChannel(streamCtx, source.Channel, &cursor)
	startTimer.Stop()
	if err != nil {
		c.setStatus(source.Channel, "source_unavailable")
		return
	}
	if events == nil || errs == nil {
		c.setStatus(source.Channel, "source_contract_refused")
		return
	}
	defer func() {
		cancel()
		// The public client closes both owned channels when its reader exits.
		// Drain only this subscription after cancellation to join that reader.
		for events != nil || errs != nil {
			select {
			case _, ok := <-events:
				if !ok {
					events = nil
				}
			case _, ok := <-errs:
				if !ok {
					errs = nil
				}
			}
		}
	}()
	c.setStatus(source.Channel, "watching")
	for {
		select {
		case <-ctx.Done():
			return
		case message, ok := <-events:
			if !ok {
				c.setStatus(source.Channel, "source_disconnected")
				return
			}
			if c.process(ctx, source, message) != nil {
				return
			}
		case _, ok := <-errs:
			if !ok {
				errs = nil
			} else {
				c.setStatus(source.Channel, "source_disconnected")
				return
			}
		}
	}
}

func (c *Consumer) process(ctx context.Context, source Source, message tether.ChannelMessage) error {
	p, err := Admit(source, message)
	if err != nil {
		c.setStatus(source.Channel, "admission_refused")
		if recordErr := c.ledger.RecordRefusal(ctx, source, message); recordErr != nil {
			return recordErr
		}
		return err
	}
	stored, err := c.ledger.Capture(ctx, p)
	if err != nil {
		c.setStatus(source.Channel, "ledger_pending")
		return err
	}
	if stored.Settled {
		return nil
	}
	cursor, _, err := c.ledger.Cursor(ctx, source)
	if err != nil {
		c.setStatus(source.Channel, "ledger_pending")
		return err
	}
	if stored.Purged {
		err = c.ledger.SettlePurge(ctx, source, p.MessageID, cursor)
	} else {
		if len(stored.Prepared) == 0 {
			input := pipeline.Message{Identity: pipeline.Identity{Source: source.key(), Message: p.MessageID}, Original: p.Original, Attribution: pipeline.Attribution{Sender: p.SenderURN, AgentID: p.AgentID, SessionID: p.SessionID, TurnID: p.TurnID, OutputID: p.OutputID}}
			settlement, runErr := c.processor.Run(ctx, input, pipeline.State{})
			if runErr != nil || settlement.Disposition != pipeline.Pass {
				c.setStatus(source.Channel, "processing_pending")
				if runErr != nil {
					return runErr
				}
				return pipeline.ErrPending
			}
			payload, prepErr := c.sink.Prepare(p, settlement.State)
			if prepErr != nil {
				c.setStatus(source.Channel, "sink_contract_refused")
				return prepErr
			}
			if err = c.ledger.Prepare(ctx, source, p.MessageID, payload); err != nil {
				c.setStatus(source.Channel, "ledger_pending")
				return err
			}
			stored.Prepared = payload
		}
		request, cancel := context.WithTimeout(ctx, c.config.RequestTimeout())
		receipt, deliverErr := c.sink.Deliver(request, stored.Prepared)
		cancel()
		if deliverErr != nil {
			c.setStatus(source.Channel, "delivery_pending")
			return deliverErr
		}
		err = c.ledger.Settle(ctx, source, p.MessageID, cursor, receipt)
	}
	if err != nil {
		c.setStatus(source.Channel, "ledger_pending")
		return err
	}
	return nil
}
