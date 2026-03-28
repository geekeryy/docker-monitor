package store

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/geekeryy/docker-monitor/internal/model"
)

type BestEffortStore struct {
	store  batchSink
	logger *slog.Logger
	name   string
	queue  chan model.LogBatch

	mu         sync.Mutex
	closed     bool
	closeOnce  sync.Once
	workerDone chan struct{}
}

const bestEffortQueueSize = 128
const bestEffortCloseTimeout = 15 * time.Second

func NewBestEffortStore(name string, store batchSink, logger *slog.Logger) *BestEffortStore {
	if isNilBatchSink(store) {
		return nil
	}
	if logger == nil {
		logger = slog.Default()
	}
	queue := make(chan model.LogBatch, bestEffortQueueSize)
	return &BestEffortStore{
		store:  store,
		logger: logger,
		name:   name,
		queue:  queue,
		workerDone: startBestEffortWorker(
			queue,
			store,
			logger,
			name,
		),
	}
}

func (s *BestEffortStore) AppendBatch(ctx context.Context, batch model.LogBatch) error {
	_ = ctx

	s.mu.Lock()
	defer s.mu.Unlock()

	if s.closed {
		s.logger.Warn("best effort sink closed, dropping batch",
			slog.String("sink", s.name),
			slog.String("log_id", batch.LogID),
		)
		return nil
	}

	select {
	case s.queue <- cloneLogBatch(batch):
		return nil
	default:
		s.logger.Warn("best effort sink queue full, dropping batch",
			slog.String("sink", s.name),
			slog.String("log_id", batch.LogID),
		)
		return nil
	}
}

func (s *BestEffortStore) Close() error {
	s.closeOnce.Do(func() {
		s.mu.Lock()
		if !s.closed {
			s.closed = true
			close(s.queue)
		}
		s.mu.Unlock()
	})

	select {
	case <-s.workerDone:
		return nil
	case <-time.After(bestEffortCloseTimeout):
		return fmt.Errorf("best effort sink %s shutdown timed out after %s", s.name, bestEffortCloseTimeout)
	}
}

func startBestEffortWorker(queue chan model.LogBatch, store batchSink, logger *slog.Logger, name string) chan struct{} {
	done := make(chan struct{})
	go func() {
		defer close(done)
		for batch := range queue {
			if err := store.AppendBatch(context.Background(), batch); err != nil {
				logger.Warn("best effort sink failed",
					slog.String("sink", name),
					slog.String("log_id", batch.LogID),
					slog.String("error", err.Error()),
				)
			}
		}
	}()
	return done
}

func cloneLogBatch(batch model.LogBatch) model.LogBatch {
	cloned := batch
	if batch.Containers != nil {
		cloned.Containers = append([]string(nil), batch.Containers...)
	}
	if batch.Events != nil {
		cloned.Events = append([]model.LogEvent(nil), batch.Events...)
	}
	return cloned
}
