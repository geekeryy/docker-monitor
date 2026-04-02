package docker

import (
	"context"
	"testing"
	"time"
)

func TestContainerLogsRequiresContainerID(t *testing.T) {
	t.Parallel()

	client := &Client{}

	if _, err := client.ContainerLogs(context.Background(), "   ", ContainerLogsOptions{}); err == nil {
		t.Fatal("ContainerLogs() error = nil, want non-nil for empty container id")
	}
}

func TestFormatSincePreservesSubSecondPrecision(t *testing.T) {
	t.Parallel()

	ts := time.Date(2026, 3, 28, 12, 34, 56, 789123456, time.UTC)
	if got := FormatSince(ts); got != "1774701296" {
		t.Fatalf("FormatSince() = %q, want unix timestamp in seconds", got)
	}
}

func TestFormatSinceOmitsFractionWhenNoSubSecondPrecision(t *testing.T) {
	t.Parallel()

	ts := time.Date(2026, 3, 28, 12, 34, 56, 0, time.UTC)
	if got := FormatSince(ts); got != "1774701296" {
		t.Fatalf("FormatSince() = %q, want unix timestamp in seconds", got)
	}
}

func TestWithRequestTimeoutAddsDeadlineWhenMissing(t *testing.T) {
	t.Parallel()

	ctx, cancel := withRequestTimeout(context.Background(), 5*time.Second)
	defer cancel()

	deadline, ok := ctx.Deadline()
	if !ok {
		t.Fatal("Deadline() ok = false, want true")
	}
	if remaining := time.Until(deadline); remaining <= 0 || remaining > 5*time.Second {
		t.Fatalf("time until deadline = %s, want within (0, 5s]", remaining)
	}
}

func TestWithRequestTimeoutKeepsExistingDeadline(t *testing.T) {
	t.Parallel()

	parent, parentCancel := context.WithTimeout(context.Background(), time.Second)
	defer parentCancel()

	ctx, cancel := withRequestTimeout(parent, 5*time.Second)
	defer cancel()

	originalDeadline, _ := parent.Deadline()
	gotDeadline, ok := ctx.Deadline()
	if !ok {
		t.Fatal("Deadline() ok = false, want true")
	}
	if !gotDeadline.Equal(originalDeadline) {
		t.Fatalf("deadline = %s, want %s", gotDeadline, originalDeadline)
	}
}
