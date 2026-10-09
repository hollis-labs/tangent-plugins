package messaging

import (
	"context"
	"sync"

	pipeline "github.com/hollis-labs/libs/message-pipeline"
)

// OwnedProcessor adds an incarnation-owned join to the portable runner. The
// runner can return pending while a cancelled stage is still unwinding; that
// return alone never permits the plugin to close its ledger or HTTP ports.
type OwnedProcessor struct {
	runner  *pipeline.Runner
	mu      sync.Mutex
	closing bool
	workers sync.WaitGroup
}

func NewProcessor(specs []pipeline.StageSpec, ledger *Ledger) (*OwnedProcessor, error) {
	owned := &OwnedProcessor{}
	specs = append([]pipeline.StageSpec(nil), specs...)
	for index := range specs {
		stage := specs[index].Stage
		if stage == nil {
			return nil, pipeline.ErrInvalid
		}
		specs[index].Stage = pipeline.StageFunc(func(ctx context.Context, in pipeline.Input) (pipeline.Result, error) {
			owned.mu.Lock()
			if owned.closing || ctx.Err() != nil {
				owned.mu.Unlock()
				return pipeline.Result{}, pipeline.ErrPending
			}
			owned.workers.Add(1)
			owned.mu.Unlock()
			defer owned.workers.Done()
			return stage.Run(ctx, in)
		})
	}
	runner, err := pipeline.New(specs, ledger, pipeline.Options{MaxOriginalBytes: 4 * 65536, MaxConcurrentStages: 1})
	if err != nil {
		return nil, err
	}
	owned.runner = runner
	return owned, nil
}

func (p *OwnedProcessor) Run(ctx context.Context, message pipeline.Message, state pipeline.State) (pipeline.Settlement, error) {
	return p.runner.Run(ctx, message, state)
}

// Join closes stage admission before waiting, so a late runner worker cannot
// race a zero-worker observation. A timed-out join still leaves ownership live.
func (p *OwnedProcessor) Join(ctx context.Context) error {
	p.mu.Lock()
	p.closing = true
	p.mu.Unlock()
	done := make(chan struct{})
	go func() { p.workers.Wait(); close(done) }()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
