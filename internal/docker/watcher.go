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
	client                 DiscoveryClient
	reader                 *LogReader
	includePatterns        []string
	ignoredID              string
	healthSink             healthEventSink
	healthContainer        string
	startedAt              time.Time
	initialSince           time.Duration
	reconnectDelay         time.Duration
	eventRetryDelay        time.Duration
	replacementGracePeriod time.Duration
	handler                LineHandler
	logger                 *slog.Logger

	mu     sync.Mutex
	active map[string]activeStream
	ids    map[string]string
	wg     sync.WaitGroup

	backfillController    BackfillController
	backfillThreshold     time.Duration
	backfillMaxDuration   time.Duration
	backfillMu            sync.Mutex
	activeStreamBackfills int
	backfillStartedAt     time.Time
	backfillMaxDisconnect time.Duration
}

// BackfillController is implemented by components that need to know when the
// watcher is replaying historical logs after a long disconnect. The watcher
// uses reference counting under the hood, so EnterBackfill / ExitBackfill may
// be called from multiple goroutines without the controller having to track
// concurrency itself.
type BackfillController interface {
	EnterBackfill()
	ExitBackfill() int64
}

type activeStream struct {
	id     string
	cancel context.CancelFunc
}

type healthEventSink interface {
	AppendBatch(ctx context.Context, batch model.LogBatch) error
}

type streamBackfillState struct {
	container       model.ContainerInfo
	disconnectedFor time.Duration
	reconnectAt     time.Time
	// once gates the actual exit logic so that the various exit paths
	// (realtime caught up / safety timer / runStream tearing down) only
	// release one slot of the host-wide backfill refcount.
	once sync.Once
}

const degradedFailureThreshold = 3
const maxRetryBackoffDelay = 30 * time.Second

// backfillRealtimeSlack tolerates clock skew between the docker daemon and the
// monitor when deciding whether the log stream has caught up to wall clock
// time. A log entry within this window of the reconnect time is treated as
// "fresh" so we do not get stuck in backfill mode forever when the streams
// straddle a few seconds of skew.
const backfillRealtimeSlack = 30 * time.Second

// defaultReplacementGracePeriod is how long we wait after a log stream ends
// before declaring the container truly gone. docker compose's recreate-by-rename
// flow needs a brief window to finish: kill old -> destroy old -> rename new ->
// start new. Within this window, a same-name replacement container surfaces and
// we should switch the stream over instead of logging a noisy "no longer running"
// notice followed by a second start event.
const defaultReplacementGracePeriod = 1500 * time.Millisecond

func NewWatcher(client DiscoveryClient, reader *LogReader, includePatterns []string, ignoredID string, startedAt time.Time, initialSince time.Duration, handler LineHandler, logger *slog.Logger) *Watcher {
	if logger == nil {
		logger = slog.Default()
	}
	if startedAt.IsZero() {
		startedAt = time.Now().UTC()
	}
	return &Watcher{
		client:                 client,
		reader:                 reader,
		includePatterns:        includePatterns,
		ignoredID:              strings.TrimSpace(ignoredID),
		healthContainer:        "monitor",
		startedAt:              startedAt.UTC(),
		initialSince:           initialSince,
		reconnectDelay:         3 * time.Second,
		eventRetryDelay:        3 * time.Second,
		replacementGracePeriod: defaultReplacementGracePeriod,
		handler:                handler,
		logger:                 logger,
		active:                 make(map[string]activeStream),
		ids:                    make(map[string]string),
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

// SetBackfillController wires the backfill coordinator (typically the
// aggregator) so the watcher can pause delivery to alert sinks while replaying
// historical logs after a long disconnect. Passing nil disables the feature.
func (w *Watcher) SetBackfillController(controller BackfillController) {
	w.backfillController = controller
}

// SetBackfillThresholds configures how the watcher decides to enter and forcibly
// exit a backfill window. A non-positive threshold disables backfill entirely.
// A non-positive maxDuration disables the safety timer (the watcher will only
// exit backfill once the log stream observes a timestamp that catches up to
// wall-clock time).
func (w *Watcher) SetBackfillThresholds(threshold, maxDuration time.Duration) {
	w.backfillThreshold = threshold
	w.backfillMaxDuration = maxDuration
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
			if errors.Is(err, context.Canceled) {
				return nil
			}

			syncFailures++
			retryDelay := nextRetryDelay(w.eventRetryDelay, syncFailures)
			w.logRetryFailure("docker container sync failed, retrying", err, syncFailures, retryDelay)
			if syncFailures == degradedFailureThreshold {
				w.reportHealthEvent(ctx, "docker.container_sync", "ERROR", "docker container sync entered degraded state", err, syncFailures)
			}

			if !w.waitForRetry(ctx, retryDelay) {
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
		retryDelay := nextRetryDelay(w.eventRetryDelay, eventFailures)
		w.logRetryFailure("docker event stream disconnected, retrying", err, eventFailures, retryDelay)
		if eventFailures == degradedFailureThreshold {
			w.reportHealthEvent(ctx, "docker.event_stream", "ERROR", "docker event stream entered degraded state", err, eventFailures)
		}

		if !w.waitForRetry(ctx, retryDelay) {
			return nil
		}
	}
}

func (w *Watcher) waitForRetry(ctx context.Context, delay time.Duration) bool {
	if delay <= 0 {
		select {
		case <-ctx.Done():
			return false
		default:
			return true
		}
	}

	timer := time.NewTimer(delay)
	defer timer.Stop()

	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

func nextRetryDelay(baseDelay time.Duration, failures int) time.Duration {
	return nextRetryDelayWithMax(baseDelay, maxRetryBackoffDelay, failures)
}

func nextRetryDelayWithMax(baseDelay, maxDelay time.Duration, failures int) time.Duration {
	if baseDelay <= 0 {
		return 0
	}
	if maxDelay > 0 && baseDelay > maxDelay {
		return maxDelay
	}
	if failures <= 1 {
		return baseDelay
	}

	delay := baseDelay
	for i := 1; i < failures; i++ {
		if maxDelay > 0 && delay >= maxDelay {
			return maxDelay
		}
		if maxDelay > 0 && delay > maxDelay/2 {
			return maxDelay
		}
		delay *= 2
	}

	if maxDelay > 0 && delay > maxDelay {
		return maxDelay
	}
	return delay
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

func (w *Watcher) logRetryFailure(msg string, err error, failures int, retryDelay time.Duration) {
	attrs := []any{
		slog.Int("consecutive_failures", failures),
	}
	if retryDelay > 0 {
		attrs = append(attrs, slog.Duration("retry_in", retryDelay))
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
	case "start", "restart", "unpause", "rename":
		// rename 事件中 Actor.Attributes["name"] 是新名字。如果新名字
		// 命中 includePatterns，需要触发 maybeStart：要么是"改名进入
		// 监控范围"（之前没在监听），要么是已有 stream 但旧 key 已不再有效，
		// 由 activate 内部处理同 ID 不同 name 的迁移。
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

	var (
		disconnectedSince time.Time
		backfill          *streamBackfillState
	)

	streamHandler := func(streamCtx context.Context, raw model.RawLog) error {
		if backfill != nil && backfill.shouldExit(raw.Timestamp) {
			w.endStreamBackfill(streamCtx, backfill, "log stream caught up to realtime")
			backfill = nil
		}
		if !raw.Timestamp.IsZero() {
			lastSeen = raw.Timestamp
		}
		return w.handler(streamCtx, raw)
	}

	defer func() {
		if backfill != nil {
			w.endStreamBackfill(rootCtx, backfill, "watcher stream stopped")
		}
	}()

	for {
		if !disconnectedSince.IsZero() && w.shouldEnterBackfill(disconnectedSince) {
			backfill = w.beginStreamBackfill(info, time.Since(disconnectedSince))
		}
		disconnectedSince = time.Time{}

		err := w.reader.Stream(ctx, info, lastSeen, streamHandler)
		if errors.Is(err, context.Canceled) {
			return
		}

		if replacement, ok := w.lookupReplacement(rootCtx, info, err == nil); ok {
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
			if disconnectedSince.IsZero() {
				disconnectedSince = time.Now()
			}
			w.logger.Warn("docker log stream disconnected, retrying",
				slog.String("container", info.Name),
				slog.String("error", err.Error()),
			)
		} else if !w.handleEndedLogStream(rootCtx, info) {
			return
		}

		select {
		case <-ctx.Done():
			return
		case <-time.After(w.reconnectDelay):
		}
	}
}

func (s *streamBackfillState) shouldExit(timestamp time.Time) bool {
	if timestamp.IsZero() {
		return false
	}
	threshold := s.reconnectAt.Add(-backfillRealtimeSlack)
	return !timestamp.Before(threshold)
}

func (w *Watcher) shouldEnterBackfill(disconnectedSince time.Time) bool {
	if w.backfillController == nil {
		return false
	}
	threshold := w.backfillThreshold
	if threshold <= 0 {
		return false
	}
	return time.Since(disconnectedSince) >= threshold
}

func (w *Watcher) beginStreamBackfill(info model.ContainerInfo, disconnectDuration time.Duration) *streamBackfillState {
	state := &streamBackfillState{
		container:       info,
		disconnectedFor: disconnectDuration,
		reconnectAt:     time.Now().UTC(),
	}

	w.backfillMu.Lock()
	w.activeStreamBackfills++
	isFirst := w.activeStreamBackfills == 1
	if isFirst {
		w.backfillStartedAt = state.reconnectAt
		w.backfillMaxDisconnect = disconnectDuration
	} else if disconnectDuration > w.backfillMaxDisconnect {
		w.backfillMaxDisconnect = disconnectDuration
	}
	w.backfillMu.Unlock()

	if isFirst && w.backfillController != nil {
		w.backfillController.EnterBackfill()
	}

	if w.backfillMaxDuration > 0 {
		// The safety timer fires once and unconditionally tries to exit. Even
		// if another exit path beat us to it, sync.Once below makes the call
		// a no-op, so we deliberately do NOT hold a reference to the *Timer
		// (which would otherwise race with concurrent endStreamBackfill paths
		// that want to Stop it).
		time.AfterFunc(w.backfillMaxDuration, func() {
			w.endStreamBackfill(context.Background(), state, "backfill max duration reached")
		})
	}

	w.logger.Info("docker log backfill started",
		slog.String("container", info.Name),
		slog.Duration("disconnected_for", disconnectDuration.Round(time.Second)),
	)

	return state
}

func (w *Watcher) endStreamBackfill(ctx context.Context, state *streamBackfillState, reason string) {
	if state == nil {
		return
	}
	state.once.Do(func() {
		w.backfillMu.Lock()
		if w.activeStreamBackfills > 0 {
			w.activeStreamBackfills--
		}
		last := w.activeStreamBackfills == 0
		maxDisconnect := w.backfillMaxDisconnect
		startedAt := w.backfillStartedAt
		if last {
			w.backfillStartedAt = time.Time{}
			w.backfillMaxDisconnect = 0
		}
		w.backfillMu.Unlock()

		w.logger.Info("docker log backfill ended",
			slog.String("container", state.container.Name),
			slog.String("reason", reason),
			slog.Duration("disconnected_for", state.disconnectedFor.Round(time.Second)),
		)

		if last && w.backfillController != nil {
			alertCount := w.backfillController.ExitBackfill()
			w.reportBackfillSummary(ctx, maxDisconnect, startedAt, alertCount, reason)
		}
	})
}

func (w *Watcher) reportBackfillSummary(ctx context.Context, disconnectDuration time.Duration, startedAt time.Time, alertCount int64, reason string) {
	if w.healthSink == nil {
		return
	}

	now := time.Now().UTC()
	rounded := disconnectDuration.Round(time.Second)
	message := fmt.Sprintf("docker log backfill completed: previously offline for %s, replayed %d matching events (see persisted log file)", rounded, alertCount)

	rawBuilder := strings.Builder{}
	rawBuilder.WriteString(message)
	rawBuilder.WriteString("\nbackfill_started_at=")
	rawBuilder.WriteString(startedAt.Format(time.RFC3339))
	rawBuilder.WriteString("\nbackfill_ended_at=")
	rawBuilder.WriteString(now.Format(time.RFC3339))
	rawBuilder.WriteString("\noffline_duration=")
	rawBuilder.WriteString(rounded.String())
	rawBuilder.WriteString(fmt.Sprintf("\nreplayed_alert_count=%d", alertCount))
	rawBuilder.WriteString("\nend_reason=")
	rawBuilder.WriteString(reason)

	event := model.LogEvent{
		Timestamp:    now,
		Container:    model.ContainerInfo{Name: w.healthContainer},
		Stream:       "system",
		Level:        "WARN",
		LogID:        "monitor.health.docker.log_backfill",
		AlertMatched: true,
		Message:      message,
		Raw:          rawBuilder.String(),
	}

	firstSeen := startedAt
	if firstSeen.IsZero() {
		firstSeen = now
	}

	batch := model.LogBatch{
		LogID:      event.LogID,
		FirstSeen:  firstSeen,
		LastSeen:   now,
		Count:      1,
		Containers: []string{event.Container.Name},
		Events:     []model.LogEvent{event},
		FlushedAt:  now,
	}

	if err := w.healthSink.AppendBatch(ctx, batch); err != nil {
		w.logger.Error("report docker log backfill summary failed",
			slog.String("error", err.Error()),
		)
	}
}

func (w *Watcher) handleEndedLogStream(ctx context.Context, info model.ContainerInfo) bool {
	container, found, inspected := w.inspectContainer(ctx, info.ID)
	if !inspected {
		w.logger.Warn("docker log stream ended unexpectedly, retrying",
			slog.String("container", info.Name),
		)
		return true
	}

	if !found {
		w.logger.Info("docker log stream ended because container no longer exists",
			slog.String("container", info.Name),
			slog.String("container_id", info.ID),
		)
		return false
	}

	if container.State.Running {
		w.logger.Warn("docker log stream ended while container is still running, retrying",
			slog.String("container", info.Name),
			slog.String("container_id", info.ID),
			slog.String("container_status", strings.TrimSpace(container.State.Status)),
		)
		return true
	}

	w.logger.Info("docker log stream ended because container is no longer running",
		slog.String("container", info.Name),
		slog.String("container_id", info.ID),
		slog.String("container_status", strings.TrimSpace(container.State.Status)),
		slog.Int("exit_code", container.State.ExitCode),
	)
	return false
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

	var cancelPrevious context.CancelFunc

	if current, exists := w.active[info.Name]; exists {
		if current.id == info.ID {
			return nil, false, nil
		}
		// 同名不同 ID：旧容器要被替换。
		delete(w.ids, current.id)
		cancelPrevious = current.cancel
	} else if previousName, ok := w.ids[info.ID]; ok && previousName != info.Name {
		// 同 ID 不同名（典型场景：docker rename）。把旧 name 槽位
		// 的 stream 取消并清理，避免新旧并存导致的双重读取。
		if previous, exists := w.active[previousName]; exists && previous.id == info.ID {
			delete(w.active, previousName)
			cancelPrevious = previous.cancel
		}
		delete(w.ids, info.ID)
	}

	streamCtx, cancel := context.WithCancel(ctx)
	w.active[info.Name] = activeStream{id: info.ID, cancel: cancel}
	w.ids[info.ID] = info.Name
	return streamCtx, true, cancelPrevious
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

// lookupReplacement decides whether a same-name replacement container exists for
// the stream that just ended. When the stream ended naturally (graceful EOF),
// we additionally tolerate docker compose's recreate window by re-checking after
// a short grace period if the first lookup came up empty.
func (w *Watcher) lookupReplacement(ctx context.Context, info model.ContainerInfo, gracefulEOF bool) (model.ContainerInfo, bool) {
	if replacement, ok := w.findReplacementContainer(ctx, info.Name, info.ID); ok {
		return replacement, true
	}

	if !gracefulEOF || w.replacementGracePeriod <= 0 {
		return model.ContainerInfo{}, false
	}

	if !w.waitForRetry(ctx, w.replacementGracePeriod) {
		return model.ContainerInfo{}, false
	}

	return w.findReplacementContainer(ctx, info.Name, info.ID)
}

// findReplacementContainer scans every container (running or otherwise) for a
// same-name candidate that is alive (or about to be) but distinct from the one
// whose stream just ended. Stale same-name containers in exited/dead state are
// ignored to avoid mistaking the just-stopped container itself or its short
// lived rename artifact for a replacement.
func (w *Watcher) findReplacementContainer(ctx context.Context, name, currentID string) (model.ContainerInfo, bool) {
	containers, err := w.client.ContainerList(ctx, ContainerListOptions{All: true})
	if err != nil {
		w.logger.Warn("refresh container list failed",
			slog.String("container", name),
			slog.String("error", err.Error()),
		)
		return model.ContainerInfo{}, false
	}

	inspector, hasInspector := w.client.(ContainerInspector)

	for _, summary := range containers {
		if summary.ID == currentID {
			continue
		}

		matched := false
		for _, candidate := range summary.Names {
			if strings.TrimPrefix(candidate, "/") == name {
				matched = true
				break
			}
		}
		if !matched {
			continue
		}

		if !hasInspector {
			return model.ContainerInfo{ID: summary.ID, Name: name}, true
		}

		container, err := inspector.ContainerInspect(ctx, summary.ID)
		if err != nil {
			if isContainerNotFound(err) {
				continue
			}
			w.logger.Warn("inspect candidate replacement container failed",
				slog.String("container", name),
				slog.String("container_id", summary.ID),
				slog.String("error", err.Error()),
			)
			continue
		}
		if !isLiveReplacement(container.State) {
			continue
		}
		return model.ContainerInfo{ID: summary.ID, Name: name}, true
	}

	return model.ContainerInfo{}, false
}

// isLiveReplacement reports whether the container state qualifies as an
// active or imminently active stand-in for the just-ended stream. We accept
// both already-running and not-yet-started states (created/restarting) so the
// watcher can pick up the new container the moment docker compose's
// recreate-by-rename flow surfaces it.
func isLiveReplacement(state ContainerState) bool {
	if state.Running || state.Restarting {
		return true
	}
	switch strings.ToLower(strings.TrimSpace(state.Status)) {
	case "running", "restarting", "created", "paused":
		return true
	}
	return false
}

func (w *Watcher) inspectContainer(ctx context.Context, id string) (ContainerJSON, bool, bool) {
	inspector, ok := w.client.(ContainerInspector)
	if !ok {
		return ContainerJSON{}, false, false
	}

	container, err := inspector.ContainerInspect(ctx, id)
	if err != nil {
		if isContainerNotFound(err) {
			return ContainerJSON{}, false, true
		}
		w.logger.Warn("inspect container after log stream ended failed",
			slog.String("container_id", id),
			slog.String("error", err.Error()),
		)
		return ContainerJSON{}, false, false
	}

	return container, true, true
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
