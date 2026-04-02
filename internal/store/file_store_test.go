package store

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/geekeryy/docker-monitor/internal/model"
)

func TestFileStoreWritesSingleDailyFileInMonthlyDir(t *testing.T) {
	t.Parallel()

	baseDir := t.TempDir()
	s := NewFileStore(baseDir)

	err := s.AppendBatch(context.Background(), model.LogBatch{
		LogID:     "warn-1",
		LastSeen:  time.Date(2026, 3, 24, 10, 0, 0, 0, time.UTC),
		Count:     1,
		FlushedAt: time.Now().UTC(),
	})
	if err != nil {
		t.Fatalf("AppendBatch(first) error = %v", err)
	}

	err = s.AppendBatch(context.Background(), model.LogBatch{
		LogID:     "warn-2",
		LastSeen:  time.Date(2026, 3, 24, 11, 0, 0, 0, time.UTC),
		Count:     1,
		FlushedAt: time.Now().UTC(),
	})
	if err != nil {
		t.Fatalf("AppendBatch(second) error = %v", err)
	}

	entries, err := os.ReadDir(filepath.Join(baseDir, "2026-03"))
	if err != nil {
		t.Fatalf("ReadDir() error = %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("len(entries) = %d, want 1", len(entries))
	}

	content, err := os.ReadFile(filepath.Join(baseDir, "2026-03", "2026-03-24.jsonl"))
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}

	lines := strings.Split(strings.TrimSpace(string(content)), "\n")
	if len(lines) != 2 {
		t.Fatalf("len(lines) = %d, want 2", len(lines))
	}
}

func TestFileStoreWritesHealthEventsToSeparateDailyFile(t *testing.T) {
	t.Parallel()

	baseDir := t.TempDir()
	s := NewFileStore(baseDir)

	ts := time.Date(2026, 4, 1, 10, 0, 0, 0, time.UTC)
	err := s.AppendBatch(context.Background(), model.LogBatch{
		LogID:     "warn-1",
		LastSeen:  ts,
		Count:     1,
		FlushedAt: time.Now().UTC(),
	})
	if err != nil {
		t.Fatalf("AppendBatch(normal) error = %v", err)
	}

	err = s.AppendBatch(context.Background(), model.LogBatch{
		LogID:     "monitor.health.docker.event_stream",
		LastSeen:  ts,
		Count:     1,
		FlushedAt: time.Now().UTC(),
	})
	if err != nil {
		t.Fatalf("AppendBatch(health) error = %v", err)
	}

	entries, err := os.ReadDir(filepath.Join(baseDir, "2026-04"))
	if err != nil {
		t.Fatalf("ReadDir() error = %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("len(entries) = %d, want 2", len(entries))
	}

	if _, err := os.Stat(filepath.Join(baseDir, "2026-04", "2026-04-01.jsonl")); err != nil {
		t.Fatalf("Stat(normal file) error = %v", err)
	}
	if _, err := os.Stat(filepath.Join(baseDir, "2026-04", "2026-04-01.health.jsonl")); err != nil {
		t.Fatalf("Stat(health file) error = %v", err)
	}
}
