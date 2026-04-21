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

func TestAggregatorBackfillModeSuppressesAlertSinksAndCountsAlerts(t *testing.T) {
	t.Parallel()

	store := &memoryStore{}
	agg := New(store, 2, time.Minute, "unknown")

	agg.EnterBackfill()
	if !agg.IsBackfilling() {
		t.Fatal("aggregator should report backfill in progress after EnterBackfill")
	}

	first := model.LogEvent{
		Timestamp:    time.Unix(200, 0).UTC(),
		LogID:        "same-id",
		AlertMatched: true,
		Container:    model.ContainerInfo{Name: "app-1"},
	}
	second := model.LogEvent{
		Timestamp:    time.Unix(201, 0).UTC(),
		LogID:        "same-id",
		AlertMatched: true,
		Container:    model.ContainerInfo{Name: "app-2"},
	}
	unknownAlert := model.LogEvent{
		Timestamp:    time.Unix(202, 0).UTC(),
		LogID:        "unknown",
		AlertMatched: true,
		Level:        "WARN",
		Container:    model.ContainerInfo{Name: "app-3"},
	}

	if err := agg.Add(context.Background(), first); err != nil {
		t.Fatalf("Add(first) error = %v", err)
	}
	if err := agg.Add(context.Background(), second); err != nil {
		t.Fatalf("Add(second) error = %v", err)
	}
	if err := agg.Add(context.Background(), unknownAlert); err != nil {
		t.Fatalf("Add(unknownAlert) error = %v", err)
	}

	if got := len(store.batches); got != 2 {
		t.Fatalf("len(store.batches) = %d, want 2", got)
	}
	for i, batch := range store.batches {
		if !batch.SuppressAlertSinks {
			t.Fatalf("batch[%d].SuppressAlertSinks = false, want true while backfill is active", i)
		}
	}

	count := agg.ExitBackfill()
	if count != 3 {
		t.Fatalf("ExitBackfill() = %d, want 3", count)
	}
	if agg.IsBackfilling() {
		t.Fatal("aggregator should not report backfill after ExitBackfill")
	}

	postEvent := model.LogEvent{
		Timestamp:    time.Unix(300, 0).UTC(),
		LogID:        "same-id",
		AlertMatched: true,
		Container:    model.ContainerInfo{Name: "app-1"},
	}
	postEvent2 := postEvent
	postEvent2.Container.Name = "app-2"
	postEvent2.Timestamp = time.Unix(301, 0).UTC()

	if err := agg.Add(context.Background(), postEvent); err != nil {
		t.Fatalf("Add(postEvent) error = %v", err)
	}
	if err := agg.Add(context.Background(), postEvent2); err != nil {
		t.Fatalf("Add(postEvent2) error = %v", err)
	}
	if got := len(store.batches); got != 3 {
		t.Fatalf("len(store.batches) after exit = %d, want 3", got)
	}
	if store.batches[2].SuppressAlertSinks {
		t.Fatalf("batch[2].SuppressAlertSinks = true after ExitBackfill, want false")
	}
}

func TestAggregatorBackfillModeRefcountedAcrossStreams(t *testing.T) {
	t.Parallel()

	store := &memoryStore{}
	agg := New(store, 10, time.Minute, "unknown")

	agg.EnterBackfill()
	agg.EnterBackfill()

	if err := agg.Add(context.Background(), model.LogEvent{
		Timestamp:    time.Unix(400, 0).UTC(),
		LogID:        "warn-1",
		AlertMatched: true,
		Container:    model.ContainerInfo{Name: "app-1"},
	}); err != nil {
		t.Fatalf("Add() error = %v", err)
	}

	if got := agg.ExitBackfill(); got != 0 {
		t.Fatalf("first ExitBackfill() = %d, want 0 because another stream is still backfilling", got)
	}
	if !agg.IsBackfilling() {
		t.Fatal("aggregator should still be backfilling after one ExitBackfill")
	}

	if err := agg.FlushAll(context.Background()); err != nil {
		t.Fatalf("FlushAll() error = %v", err)
	}
	if got := len(store.batches); got != 1 {
		t.Fatalf("len(store.batches) = %d, want 1", got)
	}
	if !store.batches[0].SuppressAlertSinks {
		t.Fatal("flushed batch should be marked SuppressAlertSinks while backfill is still active")
	}

	if got := agg.ExitBackfill(); got != 1 {
		t.Fatalf("final ExitBackfill() = %d, want 1", got)
	}
	if agg.IsBackfilling() {
		t.Fatal("aggregator should report not backfilling after both streams exit")
	}
}

func TestAggregatorExitBackfillToleratesUnderflow(t *testing.T) {
	t.Parallel()

	store := &memoryStore{}
	agg := New(store, 10, time.Minute, "unknown")

	if got := agg.ExitBackfill(); got != 0 {
		t.Fatalf("ExitBackfill() = %d, want 0 when no backfill active", got)
	}
	if agg.IsBackfilling() {
		t.Fatal("aggregator should not report backfill after underflow guard")
	}
}

func TestAggregatorBoundsBufferedEventsForKnownLogIDWithoutAlert(t *testing.T) {
	t.Parallel()

	store := &memoryStore{}
	agg := New(store, 2, time.Minute, "unknown")

	for i := 0; i < 25; i++ {
		err := agg.Add(context.Background(), model.LogEvent{
			Timestamp:    time.Unix(int64(i), 0).UTC(),
			LogID:        "known-id",
			AlertMatched: false,
			Container:    model.ContainerInfo{Name: "app-1"},
			Message:      "context",
		})
		if err != nil {
			t.Fatalf("Add(%d) error = %v", i, err)
		}
	}

	buffer := agg.groups["known-id"]
	if buffer == nil {
		t.Fatal("buffer for known-id = nil, want retained context")
	}
	if got, want := len(buffer.events), agg.maxGroupSize; got != want {
		t.Fatalf("len(buffer.events) = %d, want %d", got, want)
	}
	if got := buffer.events[0].Timestamp; !got.Equal(time.Unix(5, 0).UTC()) {
		t.Fatalf("first retained timestamp = %s, want %s", got, time.Unix(5, 0).UTC())
	}
}
