package store

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/geekeryy/docker-monitor/internal/model"
)

func TestDingTalkStoreAppendBatchWarnDoesNotMention(t *testing.T) {
	t.Parallel()

	var receivedPath string
	var receivedPayloads []dingTalkPayload

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedPath = r.URL.String()
		var payload dingTalkPayload
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Fatalf("Decode() error = %v", err)
		}
		receivedPayloads = append(receivedPayloads, payload)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"errcode":0,"errmsg":"ok"}`))
	}))
	defer server.Close()

	store := NewDingTalkStore(server.URL+"?access_token=test-token", "test-secret", false, []string{"13800000000"}, []string{"ERROR"})
	err := store.AppendBatch(context.Background(), model.LogBatch{
		LogID:      "abc123",
		FirstSeen:  time.Date(2026, 3, 24, 10, 0, 0, 0, time.UTC),
		LastSeen:   time.Date(2026, 3, 24, 10, 1, 0, 0, time.UTC),
		Count:      2,
		Containers: []string{"local-dev-api"},
		Events: []model.LogEvent{
			{
				Timestamp: time.Date(2026, 3, 24, 10, 0, 0, 0, time.UTC),
				Container: model.ContainerInfo{Name: "local-dev-api"},
				Level:     "WARN",
				Message:   "response failed",
				Raw:       "response failed\nstacktrace line 1\nstacktrace line 2",
			},
		},
	})
	if err != nil {
		t.Fatalf("AppendBatch() error = %v", err)
	}

	if !strings.Contains(receivedPath, "access_token=test-token") {
		t.Fatalf("receivedPath = %q, want access_token", receivedPath)
	}
	if !strings.Contains(receivedPath, "timestamp=") {
		t.Fatalf("receivedPath = %q, want timestamp", receivedPath)
	}
	if !strings.Contains(receivedPath, "sign=") {
		t.Fatalf("receivedPath = %q, want sign", receivedPath)
	}
	if got := len(receivedPayloads); got != 1 {
		t.Fatalf("len(receivedPayloads) = %d, want 1", got)
	}

	receivedPayload := receivedPayloads[0]
	if receivedPayload.MsgType != "markdown" {
		t.Fatalf("receivedPayload.MsgType = %q, want markdown", receivedPayload.MsgType)
	}
	if receivedPayload.Markdown.Title != "Docker日志告警 abc123" {
		t.Fatalf("markdown title = %q, want default title", receivedPayload.Markdown.Title)
	}
	if !strings.Contains(receivedPayload.Markdown.Text, "abc123") {
		t.Fatalf("markdown text = %q, want log id", receivedPayload.Markdown.Text)
	}
	if !strings.Contains(receivedPayload.Markdown.Text, "stacktrace line 1") {
		t.Fatalf("markdown text = %q, want detailed raw log", receivedPayload.Markdown.Text)
	}
	if !strings.Contains(receivedPayload.Markdown.Text, "```\nresponse failed\nstacktrace line 1\nstacktrace line 2\n```") {
		t.Fatalf("markdown text = %q, want fenced detailed log", receivedPayload.Markdown.Text)
	}
	if strings.Contains(receivedPayload.Markdown.Text, "@13800000000") {
		t.Fatalf("markdown text = %q, want no mobile mention", receivedPayload.Markdown.Text)
	}
	if receivedPayload.At.IsAtAll {
		t.Fatalf("receivedPayload.At.IsAtAll = true, want false")
	}
	if len(receivedPayload.At.AtMobiles) != 0 {
		t.Fatalf("receivedPayload.At.AtMobiles = %v, want empty", receivedPayload.At.AtMobiles)
	}
}

func TestDingTalkStoreAppendBatchErrorMentionsConfiguredUsers(t *testing.T) {
	t.Parallel()

	var receivedPayloads []dingTalkPayload

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload dingTalkPayload
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Fatalf("Decode() error = %v", err)
		}
		receivedPayloads = append(receivedPayloads, payload)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"errcode":0,"errmsg":"ok"}`))
	}))
	defer server.Close()

	store := NewDingTalkStore(server.URL+"?access_token=test-token", "test-secret", false, []string{"13800000000", "13900000000"}, []string{"ERROR"})
	err := store.AppendBatch(context.Background(), model.LogBatch{
		LogID:      "err-1",
		FirstSeen:  time.Date(2026, 3, 24, 10, 0, 0, 0, time.UTC),
		LastSeen:   time.Date(2026, 3, 24, 10, 1, 0, 0, time.UTC),
		Count:      1,
		Containers: []string{"coach_test/local-dev-api"},
		Events: []model.LogEvent{
			{
				Timestamp: time.Date(2026, 3, 24, 10, 0, 0, 0, time.UTC),
				Container: model.ContainerInfo{Name: "coach_test/local-dev-api"},
				Level:     "ERROR",
				Message:   "request failed",
				Raw:       "request failed\nstacktrace line 1",
			},
		},
	})
	if err != nil {
		t.Fatalf("AppendBatch() error = %v", err)
	}

	if got := len(receivedPayloads); got != 2 {
		t.Fatalf("len(receivedPayloads) = %d, want 2", got)
	}

	markdownPayload := receivedPayloads[0]
	if markdownPayload.MsgType != "markdown" {
		t.Fatalf("markdownPayload.MsgType = %q, want markdown", markdownPayload.MsgType)
	}
	if markdownPayload.Markdown.Title != "Docker日志告警 err-1 [coach_test]" {
		t.Fatalf("markdown title = %q, want docker host in title", markdownPayload.Markdown.Title)
	}
	if !strings.Contains(markdownPayload.Markdown.Text, "- 主机: `coach_test`") {
		t.Fatalf("markdown text = %q, want docker host summary", markdownPayload.Markdown.Text)
	}
	if strings.Contains(markdownPayload.Markdown.Text, "@13800000000") || strings.Contains(markdownPayload.Markdown.Text, "@13900000000") {
		t.Fatalf("markdown text = %q, want no mobile mentions in markdown body", markdownPayload.Markdown.Text)
	}

	textPayload := receivedPayloads[1]
	if textPayload.MsgType != "text" {
		t.Fatalf("textPayload.MsgType = %q, want text", textPayload.MsgType)
	}
	if got := len(textPayload.At.AtMobiles); got != 2 {
		t.Fatalf("len(textPayload.At.AtMobiles) = %d, want 2", got)
	}
	if textPayload.At.AtMobiles[0] != "13800000000" || textPayload.At.AtMobiles[1] != "13900000000" {
		t.Fatalf("textPayload.At.AtMobiles = %v, want configured mobiles", textPayload.At.AtMobiles)
	}
	if !strings.Contains(textPayload.Text.Content, "@13800000000") || !strings.Contains(textPayload.Text.Content, "@13900000000") {
		t.Fatalf("text content = %q, want mobile mentions in text body", textPayload.Text.Content)
	}
	if !strings.Contains(textPayload.Text.Content, "err-1") {
		t.Fatalf("text content = %q, want log id", textPayload.Text.Content)
	}
	if !strings.Contains(textPayload.Text.Content, "[coach_test]") {
		t.Fatalf("text content = %q, want docker host", textPayload.Text.Content)
	}
	if textPayload.At.IsAtAll {
		t.Fatalf("textPayload.At.IsAtAll = true, want false")
	}
}

func TestDingTalkStoreAppendBatchMentionsConfiguredLevel(t *testing.T) {
	t.Parallel()

	var receivedPayloads []dingTalkPayload

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload dingTalkPayload
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Fatalf("Decode() error = %v", err)
		}
		receivedPayloads = append(receivedPayloads, payload)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"errcode":0,"errmsg":"ok"}`))
	}))
	defer server.Close()

	store := NewDingTalkStore(server.URL+"?access_token=test-token", "test-secret", false, []string{"13800000000"}, []string{"WARN"})
	err := store.AppendBatch(context.Background(), model.LogBatch{
		LogID:      "warn-1",
		FirstSeen:  time.Date(2026, 3, 24, 10, 0, 0, 0, time.UTC),
		LastSeen:   time.Date(2026, 3, 24, 10, 1, 0, 0, time.UTC),
		Count:      1,
		Containers: []string{"coach_test/local-dev-api"},
		Events: []model.LogEvent{
			{
				Timestamp: time.Date(2026, 3, 24, 10, 0, 0, 0, time.UTC),
				Container: model.ContainerInfo{Name: "coach_test/local-dev-api"},
				Level:     "WARN",
				Message:   "queue lag detected",
				Raw:       "queue lag detected",
			},
		},
	})
	if err != nil {
		t.Fatalf("AppendBatch() error = %v", err)
	}

	if got := len(receivedPayloads); got != 2 {
		t.Fatalf("len(receivedPayloads) = %d, want 2", got)
	}

	textPayload := receivedPayloads[1]
	if got := len(textPayload.At.AtMobiles); got != 1 {
		t.Fatalf("len(textPayload.At.AtMobiles) = %d, want 1", got)
	}
	if textPayload.At.AtMobiles[0] != "13800000000" {
		t.Fatalf("textPayload.At.AtMobiles = %v, want configured mobile", textPayload.At.AtMobiles)
	}
	if !strings.Contains(textPayload.Text.Content, "@13800000000") {
		t.Fatalf("text content = %q, want mobile mention in text body", textPayload.Text.Content)
	}
	if !strings.Contains(textPayload.Text.Content, "[coach_test]") {
		t.Fatalf("text content = %q, want docker host", textPayload.Text.Content)
	}
	if got := receivedPayloads[0].Markdown.Title; got != "Docker日志告警 warn-1 [coach_test]" {
		t.Fatalf("markdown title = %q, want docker host in title", got)
	}
}
