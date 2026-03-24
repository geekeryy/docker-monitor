package docker

import (
	"context"
	"io"
	"log/slog"
	"strings"
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

type recordingLogsClient struct {
	calls chan string
}

func (c *recordingLogsClient) ContainerLogs(_ context.Context, containerID string, _ ContainerLogsOptions) (io.ReadCloser, error) {
	c.calls <- containerID
	return io.NopCloser(strings.NewReader("")), nil
}
