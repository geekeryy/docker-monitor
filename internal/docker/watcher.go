package docker

import (
	"context"
	"errors"
	"fmt"
	"io"
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
	healthSink      healthEventSink
	healthContainer string
	startedAt       time.Time
	initialSince    time.Duration
	reconnectDelay  time.Duration
	eventRetryDelay time.Duration
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

type healthEventSink interface {
	AppendBatch(ctx context.Context, batch model.LogBatch) error
}

const degradedFailureThreshold = 3

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
		healthContainer: "monitor",
		startedAt:       startedAt.UTC(),
		initialSince:    initialSince,
		reconnectDelay:  3 * time.Second,
		eventRetryDelay: 3 * time.Second,
		handler:         handler,
		logger:          logger,
		active:          make(map[string]activeStream),
		ids:             make(map[string]string),
	}
}

func (w *Watcher) SetHealthSink(sink healthEventSink) {
	w.healthSink = sink
}

func (w *Watcher) SetHealthContainerName(name string) {
	name = strings.TrimSpace(name)
	if name == "" {
		return
	}
	w.healthContainer = name
}

func (w *Watcher) Run(ctx context.Context) error {
	defer func() {
		w.stopAll()
		w.wg.Wait()
	}()

	var syncFailures int
	var eventFailures int

	for {
		if err := w.syncContainers(ctx); err != nil {
			if err == nil || errors.Is(err, context.Canceled) {
				return nil
			}

			syncFailures++
			w.logRetryFailure("docker container sync failed, retrying", err, syncFailures)
			if syncFailures == degradedFailureThreshold {
				w.reportHealthEvent(ctx, "docker.container_sync", "ERROR", "docker container sync entered degraded state", err, syncFailures)
			}

			if !w.waitForEventRetry(ctx) {
				return nil
			}
			continue
		}
		if syncFailures > 0 {
			w.logRecovery("docker container sync recovered", syncFailures)
			if syncFailures >= degradedFailureThreshold {
				w.reportHealthEvent(ctx, "docker.container_sync", "INFO", "docker container sync recovered", nil, syncFailures)
			}
			syncFailures = 0
		}

		err := w.runEventLoop(ctx, func() {
			if eventFailures > 0 {
				w.logRecovery("docker event stream recovered", eventFailures)
				if eventFailures >= degradedFailureThreshold {
					w.reportHealthEvent(ctx, "docker.event_stream", "INFO", "docker event stream recovered", nil, eventFailures)
				}
				eventFailures = 0
			}
		})
		if err == nil || errors.Is(err, context.Canceled) {
			return nil
		}

		eventFailures++
		w.logRetryFailure("docker event stream disconnected, retrying", err, eventFailures)
		if eventFailures == degradedFailureThreshold {
			w.reportHealthEvent(ctx, "docker.event_stream", "ERROR", "docker event stream entered degraded state", err, eventFailures)
		}

		if !w.waitForEventRetry(ctx) {
			return nil
		}
	}
}

func (w *Watcher) waitForEventRetry(ctx context.Context) bool {
	select {
	case <-ctx.Done():
		return false
	case <-time.After(w.eventRetryDelay):
		return true
	}
}

func (w *Watcher) syncContainers(ctx context.Context) error {
	containers, err := w.client.ContainerList(ctx, ContainerListOptions{})
	if err != nil {
		return err
	}
	for _, summary := range containers {
		w.maybeStart(ctx, summary)
	}
	return nil
}

func (w *Watcher) runEventLoop(ctx context.Context, onEvent func()) error {
	msgCh, errCh := w.client.Events(ctx, EventsOptions{Filters: map[string][]string{"type": {"container"}}})
	seenEvent := false

	for {
		select {
		case <-ctx.Done():
			return nil
		case msg, ok := <-msgCh:
			if !ok {
				msgCh = nil
				if errCh == nil {
					return io.EOF
				}
				continue
			}
			if !seenEvent {
				seenEvent = true
				if onEvent != nil {
					onEvent()
				}
			}
			w.handleEvent(ctx, msg)
		case err, ok := <-errCh:
			if !ok {
				errCh = nil
				if msgCh == nil {
					return io.EOF
				}
				continue
			}
			if err == nil || errors.Is(err, context.Canceled) {
				continue
			}
			return err
		}
	}
}

func (w *Watcher) logRetryFailure(msg string, err error, failures int) {
	attrs := []any{
		slog.Int("consecutive_failures", failures),
	}
	if err != nil {
		attrs = append(attrs, slog.String("error", err.Error()))
	}

	if failures >= degradedFailureThreshold {
		w.logger.Error(msg, attrs...)
		return
	}
	w.logger.Warn(msg, attrs...)
}

func (w *Watcher) logRecovery(msg string, failures int) {
	w.logger.Info(msg, slog.Int("previous_consecutive_failures", failures))
}

func (w *Watcher) reportHealthEvent(ctx context.Context, component, level, message string, cause error, failures int) {
	if w.healthSink == nil {
		return
	}

	now := time.Now().UTC()
	raw := message
	if cause != nil {
		raw += ": " + cause.Error()
	}
	if failures > 0 {
		raw += fmt.Sprintf("\nconsecutive_failures=%d", failures)
	}

	event := model.LogEvent{
		Timestamp:    now,
		Container:    model.ContainerInfo{Name: w.healthContainer},
		Stream:       "system",
		Level:        strings.TrimSpace(strings.ToUpper(level)),
		LogID:        "monitor.health." + component,
		AlertMatched: true,
		Message:      message,
		Raw:          raw,
	}

	batch := model.LogBatch{
		LogID:      event.LogID,
		FirstSeen:  now,
		LastSeen:   now,
		Count:      1,
		Containers: []string{event.Container.Name},
		Events:     []model.LogEvent{event},
		FlushedAt:  now,
	}

	if err := w.healthSink.AppendBatch(ctx, batch); err != nil {
		w.logger.Error("report watcher health event failed",
			slog.String("component", component),
			slog.String("level", event.Level),
			slog.String("error", err.Error()),
		)
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
