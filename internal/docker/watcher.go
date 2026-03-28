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
	active map[string]activeStream
	ids    map[string]string
	wg     sync.WaitGroup
}

type activeStream struct {
	id     string
	cancel context.CancelFunc
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
		active:          make(map[string]activeStream),
		ids:             make(map[string]string),
	}
}

func (w *Watcher) Run(ctx context.Context) error {
	defer func() {
		w.stopAll()
		w.wg.Wait()
	}()

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

	streamCtx, shouldStart, cancelPrevious := w.activate(ctx, info)
	if cancelPrevious != nil {
		cancelPrevious()
	}
	if !shouldStart {
		return
	}

	w.wg.Add(1)
	go func() {
		defer w.wg.Done()
		w.runStream(ctx, streamCtx, info)
	}()
}

func (w *Watcher) runStream(rootCtx, ctx context.Context, info model.ContainerInfo) {
	defer w.removeActive(info.Name, info.ID)

	lastSeen := w.startedAt
	if w.initialSince > 0 {
		lastSeen = w.startedAt.Add(-w.initialSince)
	}
	for {
		err := w.reader.Stream(ctx, info, lastSeen, func(streamCtx context.Context, raw model.RawLog) error {
			if !raw.Timestamp.IsZero() {
				lastSeen = raw.Timestamp
			}
			return w.handler(streamCtx, raw)
		})
		if errors.Is(err, context.Canceled) {
			return
		}

		if replacement, ok := w.resolveContainerByName(rootCtx, info.Name); ok && replacement.ID != info.ID {
			w.logger.Info("docker container replaced, switching log stream",
				slog.String("container", info.Name),
				slog.String("old_container_id", info.ID),
				slog.String("new_container_id", replacement.ID),
			)
			w.maybeStart(rootCtx, ContainerSummary{
				ID:    replacement.ID,
				Names: []string{replacement.Name},
			})
			return
		}

		if err != nil {
			w.logger.Warn("docker log stream disconnected, retrying",
				slog.String("container", info.Name),
				slog.String("error", err.Error()),
			)
		} else {
			w.logger.Warn("docker log stream ended unexpectedly, retrying",
				slog.String("container", info.Name),
			)
		}

		select {
		case <-ctx.Done():
			return
		case <-time.After(w.reconnectDelay):
		}
	}
}

func (w *Watcher) stop(id string) {
	w.mu.Lock()
	name, ok := w.ids[id]
	if !ok {
		w.mu.Unlock()
		return
	}
	stream, ok := w.active[name]
	if !ok || stream.id != id {
		delete(w.ids, id)
		w.mu.Unlock()
		return
	}
	delete(w.active, name)
	delete(w.ids, id)
	w.mu.Unlock()
	stream.cancel()
}

func (w *Watcher) stopAll() {
	w.mu.Lock()
	streams := make([]context.CancelFunc, 0, len(w.active))
	for name, stream := range w.active {
		streams = append(streams, stream.cancel)
		delete(w.ids, stream.id)
		delete(w.active, name)
	}
	w.mu.Unlock()

	for _, cancel := range streams {
		cancel()
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

func (w *Watcher) activate(ctx context.Context, info model.ContainerInfo) (context.Context, bool, context.CancelFunc) {
	w.mu.Lock()
	defer w.mu.Unlock()

	if current, exists := w.active[info.Name]; exists {
		if current.id == info.ID {
			return nil, false, nil
		}
		delete(w.ids, current.id)
		streamCtx, cancel := context.WithCancel(ctx)
		w.active[info.Name] = activeStream{id: info.ID, cancel: cancel}
		w.ids[info.ID] = info.Name
		return streamCtx, true, current.cancel
	}

	streamCtx, cancel := context.WithCancel(ctx)
	w.active[info.Name] = activeStream{id: info.ID, cancel: cancel}
	w.ids[info.ID] = info.Name
	return streamCtx, true, nil
}

func (w *Watcher) removeActive(name, id string) {
	w.mu.Lock()
	defer w.mu.Unlock()

	stream, ok := w.active[name]
	if !ok || stream.id != id {
		return
	}
	delete(w.active, name)
	delete(w.ids, id)
}

func (w *Watcher) resolveContainerByName(ctx context.Context, name string) (model.ContainerInfo, bool) {
	containers, err := w.client.ContainerList(ctx, ContainerListOptions{})
	if err != nil {
		w.logger.Warn("refresh container list failed",
			slog.String("container", name),
			slog.String("error", err.Error()),
		)
		return model.ContainerInfo{}, false
	}

	for _, summary := range containers {
		for _, candidate := range summary.Names {
			trimmed := strings.TrimPrefix(candidate, "/")
			if trimmed == name {
				return model.ContainerInfo{ID: summary.ID, Name: trimmed}, true
			}
		}
	}

	return model.ContainerInfo{}, false
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
