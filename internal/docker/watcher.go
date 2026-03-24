package docker

import (
	"context"
	"errors"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/geekeryy/docker-monitor/internal/model"
)

type DiscoveryClient interface {
	ContainerList(ctx context.Context, options ContainerListOptions) ([]ContainerSummary, error)
	Events(ctx context.Context, options EventsOptions) (<-chan EventMessage, <-chan error)
}

type Watcher struct {
	client          DiscoveryClient
	reader          *LogReader
	includePatterns []string
	ignoredID       string
	startedAt       time.Time
	initialSince    time.Duration
	reconnectDelay  time.Duration
	handler         LineHandler
	logger          *slog.Logger

	mu     sync.Mutex
	active map[string]context.CancelFunc
}

func NewWatcher(client DiscoveryClient, reader *LogReader, includePatterns []string, ignoredID string, startedAt time.Time, initialSince time.Duration, handler LineHandler, logger *slog.Logger) *Watcher {
	if logger == nil {
		logger = slog.Default()
	}
	if startedAt.IsZero() {
		startedAt = time.Now().UTC()
	}
	return &Watcher{
		client:          client,
		reader:          reader,
		includePatterns: includePatterns,
		ignoredID:       strings.TrimSpace(ignoredID),
		startedAt:       startedAt.UTC(),
		initialSince:    initialSince,
		reconnectDelay:  3 * time.Second,
		handler:         handler,
		logger:          logger,
		active:          make(map[string]context.CancelFunc),
	}
}

func (w *Watcher) Run(ctx context.Context) error {
	containers, err := w.client.ContainerList(ctx, ContainerListOptions{})
	if err != nil {
		return err
	}
	for _, summary := range containers {
		w.maybeStart(ctx, summary)
	}

	msgCh, errCh := w.client.Events(ctx, EventsOptions{Filters: map[string][]string{"type": {"container"}}})

	for {
		select {
		case <-ctx.Done():
			w.stopAll()
			return nil
		case msg, ok := <-msgCh:
			if !ok {
				msgCh = nil
				continue
			}
			w.handleEvent(ctx, msg)
		case err, ok := <-errCh:
			if !ok {
				errCh = nil
				continue
			}
			if err == nil || errors.Is(err, context.Canceled) {
				continue
			}
			return err
		}

		if msgCh == nil && errCh == nil {
			return nil
		}
	}
}

func (w *Watcher) handleEvent(ctx context.Context, msg EventMessage) {
	name := strings.TrimPrefix(msg.Actor.Attributes["name"], "/")
	containerID := eventContainerID(msg)
	if w.shouldIgnore(containerID) {
		return
	}
	if name == "" || !MatchAny(name, w.includePatterns) {
		if msg.Action == "rename" || msg.Action == "die" || msg.Action == "stop" || msg.Action == "destroy" {
			w.stop(containerID)
		}
		return
	}

	switch msg.Action {
	case "start", "restart", "unpause":
		w.maybeStart(ctx, ContainerSummary{
			ID:    containerID,
			Names: []string{name},
		})
	case "die", "stop", "destroy":
		w.stop(containerID)
	}
}

func (w *Watcher) maybeStart(ctx context.Context, summary ContainerSummary) {
	info, ok := selectContainer(summary, w.includePatterns)
	if !ok || strings.TrimSpace(info.ID) == "" {
		return
	}
	if w.shouldIgnore(info.ID) {
		return
	}

	w.mu.Lock()
	if _, exists := w.active[info.ID]; exists {
		w.mu.Unlock()
		return
	}
	streamCtx, cancel := context.WithCancel(ctx)
	w.active[info.ID] = cancel
	w.mu.Unlock()

	go w.runStream(streamCtx, info)
}

func (w *Watcher) runStream(ctx context.Context, info model.ContainerInfo) {
	defer w.stop(info.ID)

	lastSeen := w.startedAt
	if w.initialSince > 0 {
		lastSeen = w.startedAt.Add(-w.initialSince)
	}
	for {
		err := w.reader.Stream(ctx, info, lastSeen, func(streamCtx context.Context, raw model.RawLog) error {
			lastSeen = raw.Timestamp
			return w.handler(streamCtx, raw)
		})
		if err == nil || errors.Is(err, context.Canceled) {
			return
		}

		w.logger.Warn("docker log stream disconnected, retrying",
			slog.String("container", info.Name),
			slog.String("error", err.Error()),
		)

		select {
		case <-ctx.Done():
			return
		case <-time.After(w.reconnectDelay):
		}
	}
}

func (w *Watcher) stop(id string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	cancel, ok := w.active[id]
	if !ok {
		return
	}
	delete(w.active, id)
	cancel()
}

func (w *Watcher) stopAll() {
	w.mu.Lock()
	defer w.mu.Unlock()
	for id, cancel := range w.active {
		cancel()
		delete(w.active, id)
	}
}

func MatchAny(name string, patterns []string) bool {
	for _, pattern := range patterns {
		if pattern == "" {
			continue
		}
		matched, err := filepath.Match(pattern, name)
		if err == nil && matched {
			return true
		}
	}
	return false
}

func eventContainerID(msg EventMessage) string {
	if strings.TrimSpace(msg.ID) != "" {
		return msg.ID
	}
	return strings.TrimSpace(msg.Actor.ID)
}

func (w *Watcher) shouldIgnore(containerID string) bool {
	return strings.TrimSpace(containerID) != "" && strings.TrimSpace(containerID) == w.ignoredID
}

func selectContainer(summary ContainerSummary, patterns []string) (model.ContainerInfo, bool) {
	for _, name := range summary.Names {
		trimmed := strings.TrimPrefix(name, "/")
		if MatchAny(trimmed, patterns) {
			return model.ContainerInfo{ID: summary.ID, Name: trimmed}, true
		}
	}
	return model.ContainerInfo{}, false
}
