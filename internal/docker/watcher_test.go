package docker

import (
	"context"
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

type recordingLogsClient struct {
	calls chan string
}

func (c *recordingLogsClient) ContainerLogs(_ context.Context, containerID string, _ ContainerLogsOptions) (io.ReadCloser, error) {
	c.calls <- containerID
	return io.NopCloser(strings.NewReader("")), nil
}

type staticDiscoveryClient struct {
	containers []ContainerSummary
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
