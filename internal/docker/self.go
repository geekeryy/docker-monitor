package docker

import (
	"context"
	"os"
	"regexp"
	"strings"
)

var containerIDPattern = regexp.MustCompile(`(?i)[0-9a-f]{12,64}`)

type ContainerInspector interface {
	ContainerInspect(ctx context.Context, containerID string) (ContainerJSON, error)
}

func DetectSelfContainerID(ctx context.Context, inspector ContainerInspector) (string, error) {
	if inspector == nil {
		return "", nil
	}

	candidates := selfContainerIDCandidates()
	return detectSelfContainerID(ctx, inspector, candidates)
}

func detectSelfContainerID(ctx context.Context, inspector ContainerInspector, candidates []string) (string, error) {
	for _, candidate := range candidates {
		container, err := inspector.ContainerInspect(ctx, candidate)
		if err != nil {
			if isContainerNotFound(err) {
				continue
			}
			return "", err
		}
		if id := strings.TrimSpace(container.ID); id != "" {
			return id, nil
		}
	}

	return "", nil
}

func selfContainerIDCandidates() []string {
	hostname := ""
	if value, err := os.Hostname(); err == nil {
		hostname = value
	}

	cgroup := ""
	if data, err := os.ReadFile("/proc/self/cgroup"); err == nil {
		cgroup = string(data)
	}

	return selfContainerIDCandidatesFrom(hostname, cgroup)
}

func selfContainerIDCandidatesFrom(hostname, cgroup string) []string {
	seen := make(map[string]struct{})
	candidates := make([]string, 0, 4)

	appendCandidate := func(value string) {
		value = strings.TrimSpace(value)
		if value == "" {
			return
		}
		value = strings.ToLower(value)
		if _, ok := seen[value]; ok {
			return
		}
		seen[value] = struct{}{}
		candidates = append(candidates, value)
	}

	appendCandidate(hostname)
	for _, match := range containerIDPattern.FindAllString(cgroup, -1) {
		appendCandidate(match)
	}

	return candidates
}

func isContainerNotFound(err error) bool {
	if err == nil {
		return false
	}

	message := err.Error()
	return strings.Contains(message, " returned 404 ") || strings.Contains(message, "No such container")
}
