package docker

import (
	"context"
	"fmt"
	"testing"
)

func TestDetectSelfContainerID(t *testing.T) {
	t.Parallel()

	inspector := fakeContainerInspector{
		containers: map[string]ContainerJSON{
			"abcd1234ef56": {ID: "abcd1234ef56fedcba65432100112233445566778899aabbccddeeff00112233"},
		},
	}

	got, err := detectSelfContainerID(
		context.Background(),
		inspector,
		[]string{"monitor-host", "abcd1234ef56"},
	)
	if err != nil {
		t.Fatalf("detectSelfContainerID() error = %v, want nil", err)
	}
	want := "abcd1234ef56fedcba65432100112233445566778899aabbccddeeff00112233"
	if got != want {
		t.Fatalf("detectSelfContainerID() = %q, want %q", got, want)
	}
}

func TestDetectSelfContainerIDIgnoresMissingContainer(t *testing.T) {
	t.Parallel()

	got, err := detectSelfContainerID(
		context.Background(),
		fakeContainerInspector{},
		[]string{"monitor-host"},
	)
	if err != nil {
		t.Fatalf("detectSelfContainerID() error = %v, want nil", err)
	}
	if got != "" {
		t.Fatalf("detectSelfContainerID() = %q, want empty string", got)
	}
}

func TestSelfContainerIDCandidates(t *testing.T) {
	t.Parallel()

	cgroup := "0::/system.slice/docker-0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef.scope\n1:name=systemd:/docker/abcdef012345\n"
	got := selfContainerIDCandidatesFrom("abcDEF012345", cgroup)
	want := []string{
		"abcdef012345",
		"0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
	}

	if len(got) != len(want) {
		t.Fatalf("selfContainerIDCandidatesFrom() len = %d, want %d (%v)", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("selfContainerIDCandidatesFrom()[%d] = %q, want %q (all=%v)", i, got[i], want[i], got)
		}
	}
}

type fakeContainerInspector struct {
	containers map[string]ContainerJSON
}

func (f fakeContainerInspector) ContainerInspect(_ context.Context, containerID string) (ContainerJSON, error) {
	if container, ok := f.containers[containerID]; ok {
		return container, nil
	}
	return ContainerJSON{}, fmt.Errorf("docker api GET /containers/%s/json returned 404 Not Found: No such container", containerID)
}
