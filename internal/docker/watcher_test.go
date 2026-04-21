package docker

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
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
		inspect: map[string]ContainerJSON{
			"new-container-id": {
				ID:   "new-container-id",
				Name: "/api",
				State: ContainerState{
					Status:  "running",
					Running: true,
				},
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
	watcher.replacementGracePeriod = 0

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
	watcher.replacementGracePeriod = 0

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
	watcher.replacementGracePeriod = 0

	watcher.maybeStart(ctx, ContainerSummary{
		ID:    "container-a",
		Names: []string{"/api"},
	})

	waitForContainerLogsCall(t, logsClient.calls, "container-a")
	waitForLogMessage(t, logHandler, slog.LevelWarn, "docker log stream ended while container is still running, retrying")
	waitForContainerLogsCall(t, logsClient.calls, "container-a")
	cancel()
}

func TestWatcherSwitchesToReplacementSurfacedDuringGracePeriod(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	logHandler := &recordingSlogHandler{}
	logger := slog.New(logHandler)
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

	var listCalls atomic.Int32
	discoveryClient := &staticDiscoveryClient{
		containersFn: func() []ContainerSummary {
			if listCalls.Add(1) <= 1 {
				return nil
			}
			return []ContainerSummary{
				{
					ID:    "new-container-id",
					Names: []string{"/api"},
				},
			}
		},
		inspect: map[string]ContainerJSON{
			"old-container-id": {
				ID:   "old-container-id",
				Name: "/api",
				State: ContainerState{
					Status:   "exited",
					Running:  false,
					ExitCode: 0,
				},
			},
			"new-container-id": {
				ID:   "new-container-id",
				Name: "/api",
				State: ContainerState{
					Status:  "created",
					Running: false,
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
	watcher.replacementGracePeriod = 50 * time.Millisecond

	watcher.maybeStart(ctx, ContainerSummary{
		ID:    "old-container-id",
		Names: []string{"/api"},
	})

	waitForContainerLogsCall(t, logsClient.calls, "old-container-id")
	waitForContainerLogsCall(t, logsClient.calls, "new-container-id")

	if logHandler.has(slog.LevelInfo, "docker log stream ended because container is no longer running") {
		t.Fatal("docker compose replacement should suppress 'no longer running' notice")
	}
}

func TestWatcherSkipsExitedSameNameRemnantWhenChoosingReplacement(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	logHandler := &recordingSlogHandler{}
	logger := slog.New(logHandler)
	logsClient := &sequenceLogsClient{
		readers: map[string]func(context.Context) io.ReadCloser{
			"old-container-id": func(context.Context) io.ReadCloser {
				return io.NopCloser(strings.NewReader(""))
			},
		},
		calls: make(chan string, 4),
	}
	discoveryClient := &staticDiscoveryClient{
		containers: []ContainerSummary{
			{
				ID:    "remnant-container-id",
				Names: []string{"/api"},
			},
		},
		inspect: map[string]ContainerJSON{
			"old-container-id": {
				ID:   "old-container-id",
				Name: "/api",
				State: ContainerState{
					Status:   "exited",
					Running:  false,
					ExitCode: 0,
				},
			},
			"remnant-container-id": {
				ID:   "remnant-container-id",
				Name: "/api",
				State: ContainerState{
					Status:   "exited",
					Running:  false,
					ExitCode: 1,
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
	watcher.replacementGracePeriod = 30 * time.Millisecond

	watcher.maybeStart(ctx, ContainerSummary{
		ID:    "old-container-id",
		Names: []string{"/api"},
	})

	waitForContainerLogsCall(t, logsClient.calls, "old-container-id")
	waitForLogMessage(t, logHandler, slog.LevelInfo, "docker log stream ended because container is no longer running")

	select {
	case got := <-logsClient.calls:
		t.Fatalf("unexpected ContainerLogs call for %q; exited remnant must not be picked as replacement", got)
	case <-time.After(80 * time.Millisecond):
	}
}

func TestIsLiveReplacement(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		state ContainerState
		want  bool
	}{
		{name: "running flag", state: ContainerState{Running: true}, want: true},
		{name: "restarting flag", state: ContainerState{Restarting: true}, want: true},
		{name: "created status", state: ContainerState{Status: "created"}, want: true},
		{name: "paused status", state: ContainerState{Status: "Paused"}, want: true},
		{name: "exited status", state: ContainerState{Status: "exited"}, want: false},
		{name: "dead status", state: ContainerState{Status: "dead"}, want: false},
		{name: "empty state", state: ContainerState{}, want: false},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := isLiveReplacement(tt.state); got != tt.want {
				t.Fatalf("isLiveReplacement(%+v) = %v, want %v", tt.state, got, tt.want)
			}
		})
	}
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

func TestNextRetryDelayWithMaxUsesExponentialBackoff(t *testing.T) {
	t.Parallel()

	baseDelay := 10 * time.Millisecond
	maxDelay := 80 * time.Millisecond

	tests := []struct {
		name     string
		failures int
		want     time.Duration
	}{
		{name: "first failure uses base delay", failures: 1, want: 10 * time.Millisecond},
		{name: "second failure doubles delay", failures: 2, want: 20 * time.Millisecond},
		{name: "third failure doubles again", failures: 3, want: 40 * time.Millisecond},
		{name: "fourth failure hits cap", failures: 4, want: 80 * time.Millisecond},
		{name: "later failures stay capped", failures: 5, want: 80 * time.Millisecond},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := nextRetryDelayWithMax(baseDelay, maxDelay, tt.failures); got != tt.want {
				t.Fatalf("nextRetryDelayWithMax(%s, %s, %d) = %s, want %s", baseDelay, maxDelay, tt.failures, got, tt.want)
			}
		})
	}
}

func TestNextRetryDelayWithMaxResetsToBaseAfterRecovery(t *testing.T) {
	t.Parallel()

	baseDelay := 10 * time.Millisecond
	maxDelay := 80 * time.Millisecond

	if got := nextRetryDelayWithMax(baseDelay, maxDelay, 3); got != 40*time.Millisecond {
		t.Fatalf("nextRetryDelayWithMax() before recovery = %s, want %s", got, 40*time.Millisecond)
	}

	// A successful reconnect resets the consecutive failure count, so the next failure starts from the base delay again.
	if got := nextRetryDelayWithMax(baseDelay, maxDelay, 1); got != baseDelay {
		t.Fatalf("nextRetryDelayWithMax() after recovery reset = %s, want %s", got, baseDelay)
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

func TestWatcherEntersAndExitsBackfillAfterLongDisconnect(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	controller := &recordingBackfillController{alertCount: 7}
	healthSink := &recordingHealthSink{batches: make(chan model.LogBatch, 4)}

	replayLine := fmt.Sprintf("%s replayed historical alert\n", time.Now().UTC().Format(time.RFC3339Nano))

	var callCount atomic.Int32
	logsClient := &funcLogsClient{
		fn: func(ctx context.Context, _ string, _ ContainerLogsOptions) (io.ReadCloser, error) {
			switch callCount.Add(1) {
			case 1:
				return nil, errors.New("simulated ssh dial error")
			case 2:
				return io.NopCloser(strings.NewReader(replayLine)), nil
			default:
				return &contextBoundReadCloser{ctx: ctx}, nil
			}
		},
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
		slog.Default(),
	)
	watcher.reconnectDelay = 60 * time.Millisecond
	watcher.SetHealthSink(healthSink)
	watcher.SetHealthContainerName("prod-a/monitor")
	watcher.SetBackfillController(controller)
	watcher.SetBackfillThresholds(40*time.Millisecond, 5*time.Second)

	watcher.maybeStart(ctx, ContainerSummary{
		ID:    "container-a",
		Names: []string{"/api"},
	})

	batch := waitForHealthBatch(t, healthSink.batches, "monitor.health.docker.log_backfill")

	if got := controller.enters.Load(); got != 1 {
		t.Fatalf("EnterBackfill called %d times, want 1", got)
	}
	if got := controller.exits.Load(); got != 1 {
		t.Fatalf("ExitBackfill called %d times, want 1", got)
	}

	if len(batch.Containers) == 0 || batch.Containers[0] != "prod-a/monitor" {
		t.Fatalf("backfill batch container = %v, want [prod-a/monitor]", batch.Containers)
	}
	if len(batch.Events) != 1 {
		t.Fatalf("backfill batch event count = %d, want 1", len(batch.Events))
	}
	event := batch.Events[0]
	if event.Level != "WARN" {
		t.Fatalf("backfill event level = %q, want WARN", event.Level)
	}
	if !strings.Contains(event.Message, "previously offline") {
		t.Fatalf("backfill event message = %q, want disconnect duration", event.Message)
	}
	if !strings.Contains(event.Raw, "replayed_alert_count=7") {
		t.Fatalf("backfill event raw = %q, want replayed_alert_count=7", event.Raw)
	}
	if !strings.Contains(event.Raw, "end_reason=") {
		t.Fatalf("backfill event raw = %q, want end_reason annotation", event.Raw)
	}

	cancel()
}

func TestWatcherForceExitsBackfillWhenMaxDurationReached(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	controller := &recordingBackfillController{alertCount: 0}
	healthSink := &recordingHealthSink{batches: make(chan model.LogBatch, 4)}

	var callCount atomic.Int32
	logsClient := &funcLogsClient{
		fn: func(ctx context.Context, _ string, _ ContainerLogsOptions) (io.ReadCloser, error) {
			switch callCount.Add(1) {
			case 1:
				return nil, errors.New("simulated ssh dial error")
			default:
				return &contextBoundReadCloser{ctx: ctx}, nil
			}
		},
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
		slog.Default(),
	)
	watcher.reconnectDelay = 60 * time.Millisecond
	watcher.SetHealthSink(healthSink)
	watcher.SetHealthContainerName("prod-a/monitor")
	watcher.SetBackfillController(controller)
	watcher.SetBackfillThresholds(40*time.Millisecond, 80*time.Millisecond)

	watcher.maybeStart(ctx, ContainerSummary{
		ID:    "container-a",
		Names: []string{"/api"},
	})

	batch := waitForHealthBatch(t, healthSink.batches, "monitor.health.docker.log_backfill")
	event := batch.Events[0]
	if !strings.Contains(event.Raw, "end_reason=backfill max duration reached") {
		t.Fatalf("backfill event raw = %q, want end_reason=backfill max duration reached", event.Raw)
	}
	if got := controller.exits.Load(); got != 1 {
		t.Fatalf("ExitBackfill called %d times, want 1 after max duration triggers", got)
	}
	cancel()
}

func TestWatcherSkipsBackfillForBriefDisconnect(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	controller := &recordingBackfillController{alertCount: 0}
	healthSink := &recordingHealthSink{batches: make(chan model.LogBatch, 1)}

	var callCount atomic.Int32
	logsClient := &funcLogsClient{
		fn: func(ctx context.Context, _ string, _ ContainerLogsOptions) (io.ReadCloser, error) {
			switch callCount.Add(1) {
			case 1:
				return nil, errors.New("simulated short blip")
			default:
				return &contextBoundReadCloser{ctx: ctx}, nil
			}
		},
	}
	discoveryClient := &staticDiscoveryClient{
		inspect: map[string]ContainerJSON{
			"container-a": {
				ID:    "container-a",
				Name:  "/api",
				State: ContainerState{Status: "running", Running: true},
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
		slog.Default(),
	)
	// reconnectDelay (5ms) is shorter than backfillThreshold (200ms), so the
	// watcher should never enter backfill for this short blip.
	watcher.reconnectDelay = 5 * time.Millisecond
	watcher.SetHealthSink(healthSink)
	watcher.SetBackfillController(controller)
	watcher.SetBackfillThresholds(200*time.Millisecond, time.Second)

	watcher.maybeStart(ctx, ContainerSummary{
		ID:    "container-a",
		Names: []string{"/api"},
	})

	// Allow the retry loop to run a few iterations; the threshold is 200ms but
	// the reconnect delay is 5ms, so neither EnterBackfill nor a health summary
	// should fire.
	time.Sleep(80 * time.Millisecond)
	cancel()

	if got := controller.enters.Load(); got != 0 {
		t.Fatalf("EnterBackfill called %d times for a brief disconnect, want 0", got)
	}
	select {
	case batch := <-healthSink.batches:
		t.Fatalf("unexpected health batch received for brief disconnect: %+v", batch)
	default:
	}
}

type recordingBackfillController struct {
	enters     atomic.Int32
	exits      atomic.Int32
	alertCount int64
}

func (c *recordingBackfillController) EnterBackfill() {
	c.enters.Add(1)
}

func (c *recordingBackfillController) ExitBackfill() int64 {
	c.exits.Add(1)
	return c.alertCount
}

type funcLogsClient struct {
	fn func(ctx context.Context, containerID string, options ContainerLogsOptions) (io.ReadCloser, error)
}

func (c *funcLogsClient) ContainerLogs(ctx context.Context, containerID string, options ContainerLogsOptions) (io.ReadCloser, error) {
	return c.fn(ctx, containerID, options)
}

type recordingLogsClient struct {
	calls chan string
}

func (c *recordingLogsClient) ContainerLogs(_ context.Context, containerID string, _ ContainerLogsOptions) (io.ReadCloser, error) {
	c.calls <- containerID
	return io.NopCloser(strings.NewReader("")), nil
}

type staticDiscoveryClient struct {
	containers   []ContainerSummary
	containersFn func() []ContainerSummary
	inspect      map[string]ContainerJSON
	inspectErr   map[string]error
}

func (c *staticDiscoveryClient) ContainerList(_ context.Context, _ ContainerListOptions) ([]ContainerSummary, error) {
	if c.containersFn != nil {
		return c.containersFn(), nil
	}
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
