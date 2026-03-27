package aggregator

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/geekeryy/docker-monitor/internal/model"
)

type memoryStore struct {
	mu      sync.Mutex
	batches []model.LogBatch
}

func (s *memoryStore) AppendBatch(_ context.Context, batch model.LogBatch) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.batches = append(s.batches, batch)
	return nil
}

func TestAggregatorFlushesOnSize(t *testing.T) {
	t.Parallel()

	store := &memoryStore{}
	agg := New(store, 2, time.Minute, "unknown")

	first := model.LogEvent{
		Timestamp:    time.Unix(10, 0).UTC(),
		LogID:        "same-id",
		AlertMatched: true,
		Container:    model.ContainerInfo{Name: "app-1"},
	}
	second := model.LogEvent{
		Timestamp:    time.Unix(11, 0).UTC(),
		LogID:        "same-id",
		AlertMatched: true,
		Container:    model.ContainerInfo{Name: "app-2"},
	}

	if err := agg.Add(context.Background(), first); err != nil {
		t.Fatalf("Add(first) error = %v", err)
	}
	if len(store.batches) != 0 {
		t.Fatalf("len(store.batches) = %d, want 0", len(store.batches))
	}

	if err := agg.Add(context.Background(), second); err != nil {
		t.Fatalf("Add(second) error = %v", err)
	}
	if len(store.batches) != 1 {
		t.Fatalf("len(store.batches) = %d, want 1", len(store.batches))
	}
	if store.batches[0].Count != 2 {
		t.Fatalf("batch.Count = %d, want 2", store.batches[0].Count)
	}
}

func TestAggregatorFlushAll(t *testing.T) {
	t.Parallel()

	store := &memoryStore{}
	agg := New(store, 10, time.Minute, "unknown")

	if err := agg.Add(context.Background(), model.LogEvent{
		Timestamp:    time.Unix(20, 0).UTC(),
		LogID:        "warn-1",
		AlertMatched: true,
		Container:    model.ContainerInfo{Name: "app-1"},
	}); err != nil {
		t.Fatalf("Add() error = %v", err)
	}

	if err := agg.FlushAll(context.Background()); err != nil {
		t.Fatalf("FlushAll() error = %v", err)
	}
	if len(store.batches) != 1 {
		t.Fatalf("len(store.batches) = %d, want 1", len(store.batches))
	}
	if store.batches[0].LogID != "warn-1" {
		t.Fatalf("batch.LogID = %q, want %q", store.batches[0].LogID, "warn-1")
	}
}

func TestAggregatorFlushesAllLogsOnceAlertAppears(t *testing.T) {
	t.Parallel()

	store := &memoryStore{}
	agg := New(store, 2, time.Minute, "unknown")

	first := model.LogEvent{
		Timestamp:    time.Unix(30, 0).UTC(),
		LogID:        "same-id",
		AlertMatched: false,
		Container:    model.ContainerInfo{Name: "app-1"},
		Message:      "request started",
	}
	second := model.LogEvent{
		Timestamp:    time.Unix(31, 0).UTC(),
		LogID:        "same-id",
		AlertMatched: true,
		Level:        "WARN",
		Container:    model.ContainerInfo{Name: "app-1"},
		Message:      "request failed",
	}

	if err := agg.Add(context.Background(), first); err != nil {
		t.Fatalf("Add(first) error = %v", err)
	}
	if len(store.batches) != 0 {
		t.Fatalf("len(store.batches) = %d, want 0", len(store.batches))
	}

	if err := agg.Add(context.Background(), second); err != nil {
		t.Fatalf("Add(second) error = %v", err)
	}
	if len(store.batches) != 1 {
		t.Fatalf("len(store.batches) = %d, want 1", len(store.batches))
	}
	if store.batches[0].Count != 2 {
		t.Fatalf("batch.Count = %d, want 2", store.batches[0].Count)
	}
	if store.batches[0].Events[0].Message != "request started" {
		t.Fatalf("first event message = %q, want %q", store.batches[0].Events[0].Message, "request started")
	}
}

func TestAggregatorSortsEventsByTimestampInBatch(t *testing.T) {
	t.Parallel()

	store := &memoryStore{}
	agg := New(store, 2, time.Minute, "unknown")

	later := model.LogEvent{
		Timestamp:    time.Unix(101, 0).UTC(),
		LogID:        "same-id",
		AlertMatched: true,
		Container:    model.ContainerInfo{Name: "app-1"},
		Message:      "later event",
	}
	earlier := model.LogEvent{
		Timestamp:    time.Unix(100, 0).UTC(),
		LogID:        "same-id",
		AlertMatched: true,
		Container:    model.ContainerInfo{Name: "app-1"},
		Message:      "earlier event",
	}

	if err := agg.Add(context.Background(), later); err != nil {
		t.Fatalf("Add(later) error = %v", err)
	}
	if err := agg.Add(context.Background(), earlier); err != nil {
		t.Fatalf("Add(earlier) error = %v", err)
	}

	if got := len(store.batches); got != 1 {
		t.Fatalf("len(store.batches) = %d, want 1", got)
	}
	if got := len(store.batches[0].Events); got != 2 {
		t.Fatalf("len(batch.Events) = %d, want 2", got)
	}
	if got := store.batches[0].Events[0].Message; got != "earlier event" {
		t.Fatalf("first event message = %q, want earlier event", got)
	}
	if got := store.batches[0].Events[1].Message; got != "later event" {
		t.Fatalf("second event message = %q, want later event", got)
	}
}

func TestAggregatorDropsGroupsWithoutAlertOnFlush(t *testing.T) {
	t.Parallel()

	store := &memoryStore{}
	agg := New(store, 10, time.Minute, "unknown")

	if err := agg.Add(context.Background(), model.LogEvent{
		Timestamp:    time.Unix(50, 0).UTC(),
		LogID:        "info-only",
		AlertMatched: false,
		Container:    model.ContainerInfo{Name: "app-1"},
	}); err != nil {
		t.Fatalf("Add() error = %v", err)
	}

	if err := agg.FlushAll(context.Background()); err != nil {
		t.Fatalf("FlushAll() error = %v", err)
	}
	if len(store.batches) != 0 {
		t.Fatalf("len(store.batches) = %d, want 0", len(store.batches))
	}
}

func TestAggregatorFlushesUnknownLogIDIndividually(t *testing.T) {
	t.Parallel()

	store := &memoryStore{}
	agg := New(store, 20, time.Minute, "unknown")

	first := model.LogEvent{
		Timestamp:    time.Unix(60, 0).UTC(),
		LogID:        "unknown",
		AlertMatched: true,
		Level:        "WARN",
		Container:    model.ContainerInfo{Name: "app-1"},
		Message:      "first warning without log id",
	}
	second := model.LogEvent{
		Timestamp:    time.Unix(61, 0).UTC(),
		LogID:        "unknown",
		AlertMatched: true,
		Level:        "WARN",
		Container:    model.ContainerInfo{Name: "app-1"},
		Message:      "second warning without log id",
	}

	if err := agg.Add(context.Background(), first); err != nil {
		t.Fatalf("Add(first) error = %v", err)
	}
	if err := agg.Add(context.Background(), second); err != nil {
		t.Fatalf("Add(second) error = %v", err)
	}

	if got := len(store.batches); got != 2 {
		t.Fatalf("len(store.batches) = %d, want 2", got)
	}
	for i, batch := range store.batches {
		if batch.Count != 1 {
			t.Fatalf("batch[%d].Count = %d, want 1", i, batch.Count)
		}
		if len(batch.Events) != 1 {
			t.Fatalf("len(batch[%d].Events) = %d, want 1", i, len(batch.Events))
		}
		if batch.LogID != "unknown" {
			t.Fatalf("batch[%d].LogID = %q, want unknown", i, batch.LogID)
		}
	}
}
