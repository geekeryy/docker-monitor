package docker

import (
	"bytes"
	"encoding/binary"
	"strings"
	"testing"
	"time"
)

func TestSplitDockerTimestampParsesDockerPrefix(t *testing.T) {
	t.Parallel()

	gotTS, gotLine := splitDockerTimestamp("2026-03-28T12:34:56.789123456Z hello world")
	wantTS := time.Date(2026, 3, 28, 12, 34, 56, 789123456, time.UTC)
	if !gotTS.Equal(wantTS) {
		t.Fatalf("timestamp = %s, want %s", gotTS, wantTS)
	}
	if gotLine != "hello world" {
		t.Fatalf("line = %q, want %q", gotLine, "hello world")
	}
}

func TestSplitDockerTimestampPreservesExplicitOffset(t *testing.T) {
	t.Parallel()

	gotTS, gotLine := splitDockerTimestamp("2026-03-28T12:34:56+08:00 hello world")
	if got := gotTS.Format(time.RFC3339); got != "2026-03-28T12:34:56+08:00" {
		t.Fatalf("timestamp = %q, want preserved offset", got)
	}
	if gotLine != "hello world" {
		t.Fatalf("line = %q, want %q", gotLine, "hello world")
	}
}

func TestSplitDockerTimestampLeavesInvalidLineUntouched(t *testing.T) {
	t.Parallel()

	gotTS, gotLine := splitDockerTimestamp("not-a-timestamp hello world")
	if !gotTS.IsZero() {
		t.Fatalf("timestamp = %s, want zero time", gotTS)
	}
	if gotLine != "not-a-timestamp hello world" {
		t.Fatalf("line = %q, want original line", gotLine)
	}
}

func TestCopyDockerLogsRejectsOversizedMuxFrame(t *testing.T) {
	t.Parallel()

	header := make([]byte, 8)
	header[0] = 1
	binary.BigEndian.PutUint32(header[4:], maxDockerLogFrameSize+1)

	err := copyDockerLogs(bytes.NewReader(header), &bytes.Buffer{}, &bytes.Buffer{})
	if err == nil {
		t.Fatal("copyDockerLogs() error = nil, want oversized frame error")
	}
	if !strings.Contains(err.Error(), "docker log frame exceeds") {
		t.Fatalf("copyDockerLogs() error = %v, want frame limit error", err)
	}
}
