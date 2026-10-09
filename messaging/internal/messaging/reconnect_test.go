package messaging

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	tether "github.com/hollis-labs/go-tether-client"
)

type reconnectSource struct {
	mu            sync.Mutex
	history       []int64
	subscriptions int
	second        chan int64
	joined        atomic.Int32
}

func (s *reconnectSource) ChannelMessages(_ context.Context, _ string, options tether.ChannelMessagesOptions) (tether.ChannelMessagesResponse, error) {
	s.mu.Lock()
	s.history = append(s.history, options.Since)
	s.mu.Unlock()
	return tether.ChannelMessagesResponse{Channel: tether.Channel{Name: "owner-inbox", Address: "msg://service/local/channel/owner-inbox"}, NextSince: 1000000}, nil
}
func (s *reconnectSource) SubscribeChannel(ctx context.Context, _ string, since *int64) (<-chan tether.ChannelMessage, <-chan error, error) {
	if since == nil {
		return nil, nil, errors.New("nil replay cursor")
	}
	s.mu.Lock()
	s.subscriptions++
	attempt := s.subscriptions
	s.mu.Unlock()
	events := make(chan tether.ChannelMessage)
	errs := make(chan error, 1)
	go func() {
		defer s.joined.Add(1)
		defer close(events)
		defer close(errs)
		if attempt == 1 {
			select {
			case events <- routedMessage():
			case <-ctx.Done():
				return
			}
			errs <- errors.New("owned disconnect")
			return
		}
		s.second <- *since
		<-ctx.Done()
	}()
	return events, errs, nil
}

func TestConsumerReconnectUsesCommittedCursorAndJoinsBothReaders(t *testing.T) {
	l, _ := openTestLedger(t)
	source := &reconnectSource{second: make(chan int64, 1)}
	c, err := NewConsumer(consumerConfig(), l, source, &fakeProcessor{}, &fakeSink{})
	if err != nil {
		t.Fatal(err)
	}
	if err = c.Load(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Unload(context.Background()) }()
	select {
	case since := <-source.second:
		if since != 42 {
			t.Fatal("reconnect used receipt/hint instead of settlement", since)
		}
	case <-time.After(time.Second):
		t.Fatal("source did not reconnect")
	}
	if err = c.Unload(context.Background()); err != nil {
		t.Fatal(err)
	}
	source.mu.Lock()
	defer source.mu.Unlock()
	if len(source.history) != 2 || source.history[0] != 0 || source.history[1] != 42 || source.joined.Load() != 2 {
		t.Fatal("reconnect/join ownership", source.history, source.joined.Load())
	}
}
