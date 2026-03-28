package store

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/geekeryy/docker-monitor/internal/model"
)

func TestNewBestEffortStoreIgnoresTypedNilStore(t *testing.T) {
	t.Parallel()

	var sink *DingTalkStore
	if got := NewBestEffortStore("dingtalk", sink, nil); got != nil {
		t.Fatalf("NewBestEffortStore() = %#v, want nil", got)
	}
}

func TestNewMultiStoreFiltersTypedNilSink(t *testing.T) {
	t.Parallel()

	var dingTalkSink *DingTalkStore
	fileSink := NewFileStore(t.TempDir())

	store := NewMultiStore(dingTalkSink, fileSink)
	if got := len(store.sinks); got != 1 {
		t.Fatalf("len(store.sinks) = %d, want 1", got)
	}
}

func TestNilDingTalkStoreAppendBatchIsNoop(t *testing.T) {
	t.Parallel()

	var sink *DingTalkStore
	err := sink.AppendBatch(context.Background(), model.LogBatch{
		LogID:     "panic-repro",
		LastSeen:  time.Now().UTC(),
		FlushedAt: time.Now().UTC(),
	})
	if err != nil {
		t.Fatalf("AppendBatch() error = %v, want nil", err)
	}
}

func TestBestEffortStoreAppendBatchDoesNotBlockCaller(t *testing.T) {
	t.Parallel()

	blocked := make(chan struct{}, 1)
	released := make(chan struct{})
	sink := &blockingBatchSink{
		appendStarted: blocked,
		release:       released,
	}
	store := NewBestEffortStore("slow", sink, nil)

	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = store.AppendBatch(context.Background(), model.LogBatch{LogID: "async-1"})
	}()

	select {
	case <-done:
	case <-time.After(200 * time.Millisecond):
		t.Fatal("AppendBatch() blocked caller, want async enqueue")
	}

	select {
	case <-blocked:
	case <-time.After(time.Second):
		t.Fatal("underlying sink was not invoked")
	}

	close(released)
}

func TestBestEffortStoreAppendBatchStillQueuesCanceledContext(t *testing.T) {
	t.Parallel()

	received := make(chan string, 1)
	sink := &recordingBatchSink{received: received}
	store := NewBestEffortStore("slow", sink, nil)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if err := store.AppendBatch(ctx, model.LogBatch{LogID: "canceled-1"}); err != nil {
		t.Fatalf("AppendBatch() error = %v, want nil", err)
	}

	select {
	case got := <-received:
		if got != "canceled-1" {
			t.Fatalf("received log id = %q, want %q", got, "canceled-1")
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for canceled batch to be queued")
	}

	if err := store.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
}

func TestBestEffortStoreCloseDrainsQueuedBatches(t *testing.T) {
	t.Parallel()

	sink := &countingBatchSink{}
	store := NewBestEffortStore("slow", sink, nil)

	if err := store.AppendBatch(context.Background(), model.LogBatch{LogID: "drain-1"}); err != nil {
		t.Fatalf("AppendBatch() error = %v", err)
	}
	if err := store.AppendBatch(context.Background(), model.LogBatch{LogID: "drain-2"}); err != nil {
		t.Fatalf("AppendBatch() error = %v", err)
	}

	if err := store.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	if got := sink.Count(); got != 2 {
		t.Fatalf("sink.Count() = %d, want 2", got)
	}
}

type blockingBatchSink struct {
	mu            sync.Mutex
	appendStarted chan struct{}
	release       chan struct{}
}

func (s *blockingBatchSink) AppendBatch(_ context.Context, _ model.LogBatch) error {
	s.mu.Lock()
	started := s.appendStarted
	release := s.release
	s.mu.Unlock()

	select {
	case started <- struct{}{}:
	default:
	}
	<-release
	return nil
}

type recordingBatchSink struct {
	received chan string
}

func (s *recordingBatchSink) AppendBatch(_ context.Context, batch model.LogBatch) error {
	s.received <- batch.LogID
	return nil
}

type countingBatchSink struct {
	mu    sync.Mutex
	count int
}

func (s *countingBatchSink) AppendBatch(_ context.Context, _ model.LogBatch) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.count++
	return nil
}

func (s *countingBatchSink) Count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.count
}
