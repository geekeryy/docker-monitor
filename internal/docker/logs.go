package docker

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/geekeryy/docker-monitor/internal/model"
)

type LogsClient interface {
	ContainerLogs(ctx context.Context, containerID string, options ContainerLogsOptions) (io.ReadCloser, error)
}

type LineHandler func(ctx context.Context, raw model.RawLog) error

type LogReader struct {
	client LogsClient
}

func NewLogReader(client LogsClient) *LogReader {
	return &LogReader{client: client}
}

func (r *LogReader) Stream(ctx context.Context, containerInfo model.ContainerInfo, since time.Time, handler LineHandler) error {
	options := ContainerLogsOptions{
		ShowStdout: true,
		ShowStderr: true,
		Follow:     true,
		Timestamps: true,
	}
	if !since.IsZero() {
		options.Since = FormatSince(since)
	}

	rc, err := r.client.ContainerLogs(ctx, containerInfo.ID, options)
	if err != nil {
		return fmt.Errorf("open container logs: %w", err)
	}
	defer rc.Close()

	stdoutWriter := newLineWriter(ctx, containerInfo, "stdout", handler)
	stderrWriter := newLineWriter(ctx, containerInfo, "stderr", handler)
	defer stdoutWriter.Flush()
	defer stderrWriter.Flush()

	copyDone := make(chan error, 1)
	go func() {
		copyDone <- copyDockerLogs(rc, stdoutWriter, stderrWriter)
	}()

	select {
	case <-ctx.Done():
		_ = rc.Close()
		err := <-copyDone
		if err == nil || isClosedPipe(err) {
			return nil
		}
		return err
	case err := <-copyDone:
		if err == nil || isClosedPipe(err) {
			return nil
		}
		return err
	}
}

type lineWriter struct {
	ctx       context.Context
	container model.ContainerInfo
	stream    string
	handler   LineHandler

	mu     sync.Mutex
	buffer string
}

func newLineWriter(ctx context.Context, containerInfo model.ContainerInfo, stream string, handler LineHandler) *lineWriter {
	return &lineWriter{
		ctx:       ctx,
		container: containerInfo,
		stream:    stream,
		handler:   handler,
	}
}

func (w *lineWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	w.buffer += string(p)
	for {
		idx := strings.IndexByte(w.buffer, '\n')
		if idx < 0 {
			break
		}
		line := strings.TrimRight(w.buffer[:idx], "\r")
		w.buffer = w.buffer[idx+1:]
		if err := w.emit(line); err != nil {
			return 0, err
		}
	}

	return len(p), nil
}

func (w *lineWriter) Flush() error {
	w.mu.Lock()
	defer w.mu.Unlock()

	if w.buffer == "" {
		return nil
	}
	line := strings.TrimRight(w.buffer, "\r")
	w.buffer = ""
	return w.emit(line)
}

func (w *lineWriter) emit(line string) error {
	if strings.TrimSpace(line) == "" {
		return nil
	}

	ts, content := splitDockerTimestamp(line)
	return w.handler(w.ctx, model.RawLog{
		Timestamp: ts,
		Container: w.container,
		Stream:    w.stream,
		Line:      content,
	})
}

func splitDockerTimestamp(line string) (time.Time, string) {
	parts := strings.SplitN(line, " ", 2)
	if len(parts) == 2 {
		if ts, err := time.Parse(time.RFC3339Nano, parts[0]); err == nil {
			return ts.UTC(), parts[1]
		}
	}
	return time.Now().UTC(), line
}

func isClosedPipe(err error) bool {
	return errors.Is(err, io.ErrClosedPipe) || strings.Contains(err.Error(), "file already closed")
}

func copyDockerLogs(src io.Reader, stdoutWriter, stderrWriter io.Writer) error {
	reader := bufio.NewReader(src)
	header := make([]byte, 8)
	n, err := io.ReadFull(reader, header)
	if err != nil {
		if errors.Is(err, io.EOF) {
			return nil
		}
		if errors.Is(err, io.ErrUnexpectedEOF) {
			_, writeErr := stdoutWriter.Write(header[:n])
			return writeErr
		}
		return err
	}

	if !looksLikeMuxHeader(header) {
		_, err := io.Copy(stdoutWriter, io.MultiReader(bytes.NewReader(header), reader))
		return err
	}

	for {
		streamType := header[0]
		size := binary.BigEndian.Uint32(header[4:])
		payload := make([]byte, size)
		if _, err := io.ReadFull(reader, payload); err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
				return nil
			}
			return err
		}

		var target io.Writer
		switch streamType {
		case 1:
			target = stdoutWriter
		case 2:
			target = stderrWriter
		default:
			target = stdoutWriter
		}

		if _, err := target.Write(payload); err != nil {
			return err
		}

		if _, err := io.ReadFull(reader, header); err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			if errors.Is(err, io.ErrUnexpectedEOF) {
				return nil
			}
			return err
		}
	}
}

func looksLikeMuxHeader(header []byte) bool {
	return len(header) == 8 && (header[0] == 1 || header[0] == 2) && header[1] == 0 && header[2] == 0 && header[3] == 0
}
