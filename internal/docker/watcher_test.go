package docker

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/geekeryy/docker-monitor/internal/model"
)

func TestMatchAny(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		target   string
		patterns []string
		want     bool
	}{
		{
			name:     "matches wildcard prefix",
			target:   "app-api-1",
			patterns: []string{"app-*"},
			want:     true,
		},
		{
			name:     "matches exact name",
			target:   "gateway",
			patterns: []string{"gateway"},
			want:     true,
		},
		{
			name:     "does not match different service",
			target:   "db-1",
			patterns: []string{"app-*", "worker-*"},
			want:     false,
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := MatchAny(tt.target, tt.patterns); got != tt.want {
				t.Fatalf("MatchAny(%q, %v) = %v, want %v", tt.target, tt.patterns, got, tt.want)
			}
		})
	}
}

func TestEventContainerID(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		msg  EventMessage
		want string
	}{
		{
			name: "uses top level id when present",
			msg: EventMessage{
				ID: "container-123",
				Actor: EventActor{
					ID: "actor-456",
				},
			},
			want: "container-123",
		},
		{
			name: "falls back to actor id when top level id missing",
			msg: EventMessage{
				Actor: EventActor{
					ID: "actor-456",
				},
			},
			want: "actor-456",
		},
		{
			name: "trims whitespace from fallback id",
			msg: EventMessage{
				Actor: EventActor{
					ID: " actor-456 ",
				},
			},
			want: "actor-456",
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := eventContainerID(tt.msg); got != tt.want {
				t.Fatalf("eventContainerID(%+v) = %q, want %q", tt.msg, got, tt.want)
			}
		})
	}
}

func TestWatcherMaybeStartIgnoresSelfContainer(t *testing.T) {
	t.Parallel()

	logsClient := &recordingLogsClient{calls: make(chan string, 1)}
	watcher := NewWatcher(
		nil,
		NewLogReader(logsClient),
		[]string{"*"},
		"self-container-id",
		time.Now(),
		0,
		func(context.Context, model.RawLog) error { return nil },
		slog.Default(),
	)

	watcher.maybeStart(context.Background(), ContainerSummary{
		ID:    "self-container-id",
		Names: []string{"/docker-monitor"},
	})

	select {
	case got := <-logsClient.calls:
		t.Fatalf("ContainerLogs called for ignored container %q", got)
	case <-time.After(100 * time.Millisecond):
	}
}

func TestWatcherHandleEventIgnoresSelfContainer(t *testing.T) {
	t.Parallel()

	logsClient := &recordingLogsClient{calls: make(chan string, 1)}
	watcher := NewWatcher(
		nil,
		NewLogReader(logsClient),
		[]string{"*"},
		"self-container-id",
		time.Now(),
		0,
		func(context.Context, model.RawLog) error { return nil },
		slog.Default(),
	)

	watcher.handleEvent(context.Background(), EventMessage{
		ID:     "self-container-id",
		Action: "start",
		Actor: EventActor{
			Attributes: map[string]string{"name": "docker-monitor"},
		},
	})

	select {
	case got := <-logsClient.calls:
		t.Fatalf("ContainerLogs called for ignored event container %q", got)
	case <-time.After(100 * time.Millisecond):
	}
}

func TestWatcherReattachesWhenContainerRecreatedWithSameName(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	discoveryClient := &staticDiscoveryClient{
		containers: []ContainerSummary{
			{
				ID:    "new-container-id",
				Names: []string{"/api"},
			},
		},
	}
	logsClient := &sequenceLogsClient{
		readers: map[string]func(context.Context) io.ReadCloser{
			"old-container-id": func(context.Context) io.ReadCloser {
				return io.NopCloser(strings.NewReader(""))
			},
			"new-container-id": func(ctx context.Context) io.ReadCloser {
				return &contextBoundReadCloser{ctx: ctx}
			},
		},
		calls: make(chan string, 4),
	}
	watcher := NewWatcher(
		discoveryClient,
		NewLogReader(logsClient),
		[]string{"api"},
		"",
		time.Now(),
		0,
		func(context.Context, model.RawLog) error { return nil },
		slog.Default(),
	)

	watcher.maybeStart(ctx, ContainerSummary{
		ID:    "old-container-id",
		Names: []string{"/api"},
	})

	waitForContainerLogsCall(t, logsClient.calls, "old-container-id")
	waitForContainerLogsCall(t, logsClient.calls, "new-container-id")
}

func TestWatcherMaybeStartReplacesExistingStreamForSameName(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	logsClient := &sequenceLogsClient{
		readers: map[string]func(context.Context) io.ReadCloser{
			"container-a": func(ctx context.Context) io.ReadCloser {
				return &contextBoundReadCloser{ctx: ctx}
			},
			"container-b": func(ctx context.Context) io.ReadCloser {
				return &contextBoundReadCloser{ctx: ctx}
			},
		},
		calls: make(chan string, 4),
	}
	watcher := NewWatcher(
		&staticDiscoveryClient{},
		NewLogReader(logsClient),
		[]string{"api"},
		"",
		time.Now(),
		0,
		func(context.Context, model.RawLog) error { return nil },
		slog.Default(),
	)

	watcher.maybeStart(ctx, ContainerSummary{
		ID:    "container-a",
		Names: []string{"/api"},
	})
	waitForContainerLogsCall(t, logsClient.calls, "container-a")

	watcher.maybeStart(ctx, ContainerSummary{
		ID:    "container-b",
		Names: []string{"/api"},
	})
	waitForContainerLogsCall(t, logsClient.calls, "container-b")
}

func TestWatcherRunStreamStopsRetryingWhenContainerHasStopped(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	logHandler := &recordingSlogHandler{}
	logger := slog.New(logHandler)
	logsClient := &sequenceLogsClient{
		readers: map[string]func(context.Context) io.ReadCloser{
			"container-a": func(context.Context) io.ReadCloser {
				return io.NopCloser(strings.NewReader(""))
			},
		},
		calls: make(chan string, 4),
	}
	discoveryClient := &staticDiscoveryClient{
		inspect: map[string]ContainerJSON{
			"container-a": {
				ID:   "container-a",
				Name: "/api",
				State: ContainerState{
					Status:   "exited",
					Running:  false,
					ExitCode: 137,
				},
			},
		},
	}
	watcher := NewWatcher(
		discoveryClient,
		NewLogReader(logsClient),
		[]string{"api"},
		"",
		time.Now(),
		0,
		func(context.Context, model.RawLog) error { return nil },
		logger,
	)
	watcher.reconnectDelay = 20 * time.Millisecond

	watcher.maybeStart(ctx, ContainerSummary{
		ID:    "container-a",
		Names: []string{"/api"},
	})

	waitForContainerLogsCall(t, logsClient.calls, "container-a")
	waitForLogMessage(t, logHandler, slog.LevelInfo, "docker log stream ended because container is no longer running")

	select {
	case got := <-logsClient.calls:
		t.Fatalf("unexpected retry ContainerLogs call for %q after container stopped", got)
	case <-time.After(80 * time.Millisecond):
	}
}

func TestWatcherRunStreamRetriesWhenContainerIsStillRunning(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	logHandler := &recordingSlogHandler{}
	logger := slog.New(logHandler)
	logsClient := &sequenceLogsClient{
		readers: map[string]func(context.Context) io.ReadCloser{
			"container-a": func(context.Context) io.ReadCloser {
				return io.NopCloser(strings.NewReader(""))
			},
		},
		calls: make(chan string, 8),
	}
	discoveryClient := &staticDiscoveryClient{
		inspect: map[string]ContainerJSON{
			"container-a": {
				ID:   "container-a",
				Name: "/api",
				State: ContainerState{
					Status:  "running",
					Running: true,
				},
			},
		},
	}
	watcher := NewWatcher(
		discoveryClient,
		NewLogReader(logsClient),
		[]string{"api"},
		"",
		time.Now(),
		0,
		func(context.Context, model.RawLog) error { return nil },
		logger,
	)
	watcher.reconnectDelay = 20 * time.Millisecond

	watcher.maybeStart(ctx, ContainerSummary{
		ID:    "container-a",
		Names: []string{"/api"},
	})

	waitForContainerLogsCall(t, logsClient.calls, "container-a")
	waitForLogMessage(t, logHandler, slog.LevelWarn, "docker log stream ended while container is still running, retrying")
	waitForContainerLogsCall(t, logsClient.calls, "container-a")
	cancel()
}

func TestWatcherRunReconnectsEventStream(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	logsClient := &sequenceLogsClient{
		readers: map[string]func(context.Context) io.ReadCloser{
			"container-a": func(context.Context) io.ReadCloser {
				return io.NopCloser(strings.NewReader(""))
			},
		},
		calls: make(chan string, 1),
	}
	discoveryClient := &reconnectingDiscoveryClient{}
	watcher := NewWatcher(
		discoveryClient,
		NewLogReader(logsClient),
		[]string{"api"},
		"",
		time.Now(),
		0,
		func(context.Context, model.RawLog) error { return nil },
		slog.Default(),
	)
	watcher.eventRetryDelay = 10 * time.Millisecond

	runDone := make(chan error, 1)
	go func() {
		runDone <- watcher.Run(ctx)
	}()

	waitForContainerLogsCall(t, logsClient.calls, "container-a")
	cancel()

	select {
	case err := <-runDone:
		if err != nil {
			t.Fatalf("Watcher.Run() error = %v, want nil", err)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for Watcher.Run to stop")
	}

	if got := discoveryClient.eventsCalls(); got < 2 {
		t.Fatalf("Events called %d times, want at least 2", got)
	}
}

func TestWatcherRunRetriesContainerSyncFailures(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	logsClient := &sequenceLogsClient{
		readers: map[string]func(context.Context) io.ReadCloser{
			"container-a": func(context.Context) io.ReadCloser {
				return io.NopCloser(strings.NewReader(""))
			},
		},
		calls: make(chan string, 1),
	}
	discoveryClient := &flakySyncDiscoveryClient{}
	watcher := NewWatcher(
		discoveryClient,
		NewLogReader(logsClient),
		[]string{"api"},
		"",
		time.Now(),
		0,
		func(context.Context, model.RawLog) error { return nil },
		slog.Default(),
	)
	watcher.eventRetryDelay = 10 * time.Millisecond

	runDone := make(chan error, 1)
	go func() {
		runDone <- watcher.Run(ctx)
	}()

	waitForContainerLogsCall(t, logsClient.calls, "container-a")
	cancel()

	select {
	case err := <-runDone:
		if err != nil {
			t.Fatalf("Watcher.Run() error = %v, want nil", err)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for Watcher.Run to stop")
	}

	if got := discoveryClient.containerListCalls(); got < 2 {
		t.Fatalf("ContainerList called %d times, want at least 2", got)
	}
}

func TestWatcherRunLogsDegradedAndRecoveredEventStream(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	logsClient := &sequenceLogsClient{
		calls: make(chan string, 1),
	}
	discoveryClient := &degradingEventsDiscoveryClient{}
	logHandler := &recordingSlogHandler{}
	watcher := NewWatcher(
		discoveryClient,
		NewLogReader(logsClient),
		[]string{"api"},
		"",
		time.Now(),
		0,
		func(context.Context, model.RawLog) error { return nil },
		slog.New(logHandler),
	)
	watcher.eventRetryDelay = 10 * time.Millisecond

	runDone := make(chan error, 1)
	go func() {
		runDone <- watcher.Run(ctx)
	}()

	waitForLogMessage(t, logHandler, slog.LevelError, "docker event stream disconnected, retrying")
	waitForLogMessage(t, logHandler, slog.LevelInfo, "docker event stream recovered")
	cancel()

	select {
	case err := <-runDone:
		if err != nil {
			t.Fatalf("Watcher.Run() error = %v, want nil", err)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for Watcher.Run to stop")
	}
}

func TestWatcherReportsHealthEventsOnDegradedAndRecoveredEventStream(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	discoveryClient := &degradingEventsDiscoveryClient{}
	healthSink := &recordingHealthSink{batches: make(chan model.LogBatch, 2)}
	watcher := NewWatcher(
		discoveryClient,
		NewLogReader(&sequenceLogsClient{calls: make(chan string, 1)}),
		[]string{"api"},
		"",
		time.Now(),
		0,
		func(context.Context, model.RawLog) error { return nil },
		slog.Default(),
	)
	watcher.eventRetryDelay = 10 * time.Millisecond
	watcher.SetHealthSink(healthSink)
	watcher.SetHealthContainerName("prod-a/monitor")

	runDone := make(chan error, 1)
	go func() {
		runDone <- watcher.Run(ctx)
	}()

	degraded := waitForHealthBatch(t, healthSink.batches, "monitor.health.docker.event_stream")
	if degraded.Events[0].Level != "ERROR" {
		t.Fatalf("degraded event level = %q, want ERROR", degraded.Events[0].Level)
	}
	if degraded.Containers[0] != "prod-a/monitor" {
		t.Fatalf("degraded container = %q, want prod-a/monitor", degraded.Containers[0])
	}

	recovered := waitForHealthBatch(t, healthSink.batches, "monitor.health.docker.event_stream")
	if recovered.Events[0].Level != "INFO" {
		t.Fatalf("recovered event level = %q, want INFO", recovered.Events[0].Level)
	}

	cancel()

	select {
	case err := <-runDone:
		if err != nil {
			t.Fatalf("Watcher.Run() error = %v, want nil", err)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for Watcher.Run to stop")
	}
}

type recordingLogsClient struct {
	calls chan string
}

func (c *recordingLogsClient) ContainerLogs(_ context.Context, containerID string, _ ContainerLogsOptions) (io.ReadCloser, error) {
	c.calls <- containerID
	return io.NopCloser(strings.NewReader("")), nil
}

type staticDiscoveryClient struct {
	containers []ContainerSummary
	inspect    map[string]ContainerJSON
	inspectErr map[string]error
}

func (c *staticDiscoveryClient) ContainerList(_ context.Context, _ ContainerListOptions) ([]ContainerSummary, error) {
	return c.containers, nil
}

func (c *staticDiscoveryClient) Events(_ context.Context, _ EventsOptions) (<-chan EventMessage, <-chan error) {
	msgCh := make(chan EventMessage)
	errCh := make(chan error)
	close(msgCh)
	close(errCh)
	return msgCh, errCh
}

func (c *staticDiscoveryClient) ContainerInspect(_ context.Context, containerID string) (ContainerJSON, error) {
	if err := c.inspectErr[containerID]; err != nil {
		return ContainerJSON{}, err
	}
	if container, ok := c.inspect[containerID]; ok {
		return container, nil
	}
	return ContainerJSON{}, errors.New("docker api GET /containers/" + containerID + "/json returned 404: No such container")
}

type reconnectingDiscoveryClient struct {
	mu    sync.Mutex
	calls int
}

func (c *reconnectingDiscoveryClient) ContainerList(_ context.Context, _ ContainerListOptions) ([]ContainerSummary, error) {
	return nil, nil
}

func (c *reconnectingDiscoveryClient) Events(_ context.Context, _ EventsOptions) (<-chan EventMessage, <-chan error) {
	c.mu.Lock()
	c.calls++
	call := c.calls
	c.mu.Unlock()

	msgCh := make(chan EventMessage, 1)
	errCh := make(chan error, 1)

	if call == 1 {
		errCh <- errors.New("ssh session closed")
		close(msgCh)
		close(errCh)
		return msgCh, errCh
	}

	msgCh <- EventMessage{
		ID:     "container-a",
		Action: "start",
		Actor: EventActor{
			Attributes: map[string]string{"name": "api"},
		},
	}
	close(msgCh)
	close(errCh)
	return msgCh, errCh
}

func (c *reconnectingDiscoveryClient) eventsCalls() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.calls
}

type flakySyncDiscoveryClient struct {
	mu               sync.Mutex
	containerListTry int
}

func (c *flakySyncDiscoveryClient) ContainerList(_ context.Context, _ ContainerListOptions) ([]ContainerSummary, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.containerListTry++
	if c.containerListTry == 1 {
		return nil, errors.New("ssh dial failed")
	}

	return []ContainerSummary{
		{
			ID:    "container-a",
			Names: []string{"/api"},
		},
	}, nil
}

func (c *flakySyncDiscoveryClient) Events(_ context.Context, _ EventsOptions) (<-chan EventMessage, <-chan error) {
	msgCh := make(chan EventMessage)
	errCh := make(chan error)
	close(msgCh)
	close(errCh)
	return msgCh, errCh
}

func (c *flakySyncDiscoveryClient) containerListCalls() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.containerListTry
}

type degradingEventsDiscoveryClient struct {
	mu    sync.Mutex
	calls int
}

func (c *degradingEventsDiscoveryClient) ContainerList(_ context.Context, _ ContainerListOptions) ([]ContainerSummary, error) {
	return nil, nil
}

func (c *degradingEventsDiscoveryClient) Events(ctx context.Context, _ EventsOptions) (<-chan EventMessage, <-chan error) {
	c.mu.Lock()
	c.calls++
	call := c.calls
	c.mu.Unlock()

	msgCh := make(chan EventMessage)
	errCh := make(chan error, 1)

	if call <= degradedFailureThreshold {
		errCh <- errors.New("ssh session closed")
		close(msgCh)
		close(errCh)
		return msgCh, errCh
	}

	go func() {
		msgCh <- EventMessage{Action: "noop"}
		<-ctx.Done()
		close(msgCh)
		close(errCh)
	}()
	return msgCh, errCh
}

type logRecord struct {
	level   slog.Level
	message string
}

type recordingSlogHandler struct {
	mu      sync.Mutex
	records []logRecord
}

type recordingHealthSink struct {
	batches chan model.LogBatch
}

func (s *recordingHealthSink) AppendBatch(_ context.Context, batch model.LogBatch) error {
	s.batches <- batch
	return nil
}

func (h *recordingSlogHandler) Enabled(context.Context, slog.Level) bool {
	return true
}

func (h *recordingSlogHandler) Handle(_ context.Context, record slog.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.records = append(h.records, logRecord{
		level:   record.Level,
		message: record.Message,
	})
	return nil
}

func (h *recordingSlogHandler) WithAttrs([]slog.Attr) slog.Handler {
	return h
}

func (h *recordingSlogHandler) WithGroup(string) slog.Handler {
	return h
}

func (h *recordingSlogHandler) has(level slog.Level, message string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()

	for _, record := range h.records {
		if record.level == level && record.message == message {
			return true
		}
	}
	return false
}

type sequenceLogsClient struct {
	mu      sync.Mutex
	readers map[string]func(context.Context) io.ReadCloser
	calls   chan string
}

func (c *sequenceLogsClient) ContainerLogs(ctx context.Context, containerID string, _ ContainerLogsOptions) (io.ReadCloser, error) {
	c.mu.Lock()
	readerFactory := c.readers[containerID]
	c.mu.Unlock()

	c.calls <- containerID
	if readerFactory == nil {
		return io.NopCloser(strings.NewReader("")), nil
	}
	return readerFactory(ctx), nil
}

type contextBoundReadCloser struct {
	ctx context.Context
}

func (r *contextBoundReadCloser) Read(_ []byte) (int, error) {
	<-r.ctx.Done()
	return 0, io.EOF
}

func (r *contextBoundReadCloser) Close() error {
	return nil
}

func waitForContainerLogsCall(t *testing.T, calls <-chan string, want string) {
	t.Helper()

	select {
	case got := <-calls:
		if got != want {
			t.Fatalf("ContainerLogs called for %q, want %q", got, want)
		}
	case <-time.After(time.Second):
		t.Fatalf("timed out waiting for ContainerLogs(%q)", want)
	}
}

func waitForLogMessage(t *testing.T, handler *recordingSlogHandler, level slog.Level, message string) {
	t.Helper()

	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if handler.has(level, message) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}

	t.Fatalf("timed out waiting for log %q at level %s", message, level.String())
}

func waitForHealthBatch(t *testing.T, batches <-chan model.LogBatch, logID string) model.LogBatch {
	t.Helper()

	select {
	case batch := <-batches:
		if batch.LogID != logID {
			t.Fatalf("health batch log_id = %q, want %q", batch.LogID, logID)
		}
		return batch
	case <-time.After(time.Second):
		t.Fatalf("timed out waiting for health batch %q", logID)
		return model.LogBatch{}
	}
}
