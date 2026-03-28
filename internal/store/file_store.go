package store

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/geekeryy/docker-monitor/internal/model"
)

type FileStore struct {
	baseDir string
	mu      sync.Mutex
}

func NewFileStore(baseDir string) *FileStore {
	return &FileStore{baseDir: baseDir}
}

func (s *FileStore) AppendBatch(_ context.Context, batch model.LogBatch) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	ts := batch.LastSeen.UTC()
	if ts.IsZero() {
		ts = time.Now().UTC()
	}

	dir := filepath.Join(s.baseDir, ts.Format("2006-01"))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create output dir: %w", err)
	}

	target := filepath.Join(dir, ts.Format("2006-01-02")+".jsonl")
	file, err := os.OpenFile(target, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("open output file: %w", err)
	}

	encoder := json.NewEncoder(file)
	if err := encoder.Encode(batch); err != nil {
		_ = file.Close()
		return fmt.Errorf("encode batch: %w", err)
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return fmt.Errorf("sync output file: %w", err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("close output file: %w", err)
	}

	return nil
}
