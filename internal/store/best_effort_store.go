package store

import (
	"context"
	"log/slog"

	"github.com/geekeryy/docker-monitor/internal/model"
)

type BestEffortStore struct {
	store  batchSink
	logger *slog.Logger
	name   string
}

func NewBestEffortStore(name string, store batchSink, logger *slog.Logger) *BestEffortStore {
	if isNilBatchSink(store) {
		return nil
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &BestEffortStore{
		store:  store,
		logger: logger,
		name:   name,
	}
}

func (s *BestEffortStore) AppendBatch(ctx context.Context, batch model.LogBatch) error {
	if err := s.store.AppendBatch(ctx, batch); err != nil {
		s.logger.Warn("best effort sink failed",
			slog.String("sink", s.name),
			slog.String("log_id", batch.LogID),
			slog.String("error", err.Error()),
		)
	}
	return nil
}
