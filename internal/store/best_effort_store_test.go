package store

import (
	"context"
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
