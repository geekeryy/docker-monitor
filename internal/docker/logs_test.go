package docker

import (
	"bytes"
	"context"
	"encoding/binary"
	"strings"
	"testing"
	"time"

	"github.com/geekeryy/docker-monitor/internal/model"
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

func TestLineWriterTruncatesOversizedLineAndContinues(t *testing.T) {
	t.Parallel()

	container := model.ContainerInfo{ID: "c1", Name: "coach_server-api-1"}
	var got []model.RawLog
	writer := newLineWriter(context.Background(), container, "stdout", func(_ context.Context, raw model.RawLog) error {
		got = append(got, raw)
		return nil
	})

	oversized := "2026-03-28T12:34:56Z " + strings.Repeat("a", maxDockerLogLineSize)
	payload := oversized + "\n2026-03-28T12:34:57Z ok\n"

	for _, chunk := range []string{
		payload[:maxDockerLogLineSize/2],
		payload[maxDockerLogLineSize/2 : maxDockerLogLineSize+32],
		payload[maxDockerLogLineSize+32:],
	} {
		if _, err := writer.Write([]byte(chunk)); err != nil {
			t.Fatalf("lineWriter.Write() error = %v, want nil", err)
		}
	}
	if err := writer.Flush(); err != nil {
		t.Fatalf("lineWriter.Flush() error = %v, want nil", err)
	}

	if len(got) != 2 {
		t.Fatalf("emitted log count = %d, want 2", len(got))
	}
	if got[0].Container != container {
		t.Fatalf("first container = %+v, want %+v", got[0].Container, container)
	}
	if got[0].Stream != "stdout" {
		t.Fatalf("first stream = %q, want stdout", got[0].Stream)
	}
	if !strings.HasSuffix(got[0].Line, dockerLogLineTruncatedSuffix) {
		t.Fatalf("first line missing truncation suffix: %q", got[0].Line)
	}
	if !strings.HasPrefix(got[0].Line, strings.Repeat("a", 32)) {
		t.Fatalf("first line = %q, want preserved content prefix", got[0].Line)
	}
	if got[1].Line != "ok" {
		t.Fatalf("second line = %q, want ok", got[1].Line)
	}
}
