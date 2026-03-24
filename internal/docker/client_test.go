package docker

import (
	"context"
	"testing"
)

func TestContainerLogsRequiresContainerID(t *testing.T) {
	t.Parallel()

	client := &Client{}

	if _, err := client.ContainerLogs(context.Background(), "   ", ContainerLogsOptions{}); err == nil {
		t.Fatal("ContainerLogs() error = nil, want non-nil for empty container id")
	}
}
