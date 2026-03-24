package store

import (
	"context"
	"errors"

	"github.com/geekeryy/docker-monitor/internal/model"
)

type batchSink interface {
	AppendBatch(ctx context.Context, batch model.LogBatch) error
}

type MultiStore struct {
	sinks []batchSink
}

func NewMultiStore(sinks ...batchSink) *MultiStore {
	filtered := make([]batchSink, 0, len(sinks))
	for _, sink := range sinks {
		if !isNilBatchSink(sink) {
			filtered = append(filtered, sink)
		}
	}
	return &MultiStore{sinks: filtered}
}

func (s *MultiStore) AppendBatch(ctx context.Context, batch model.LogBatch) error {
	var errs []error
	for _, sink := range s.sinks {
		if err := sink.AppendBatch(ctx, batch); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}
