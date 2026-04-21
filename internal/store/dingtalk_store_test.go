package store

import (
	"context"
	"encoding/json"
	"io"
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

	store := NewDingTalkStore(server.URL+"?access_token=test-token", "test-secret", false, []string{"13800000000"}, []string{"ERROR"}, 5)
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

	store := NewDingTalkStore(server.URL+"?access_token=test-token", "test-secret", false, []string{"13800000000", "13900000000"}, []string{"ERROR"}, 5)
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

	store := NewDingTalkStore(server.URL+"?access_token=test-token", "test-secret", false, []string{"13800000000"}, []string{"WARN"}, 5)
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

func TestBuildDingTalkMarkdownRespectsMaxEvents(t *testing.T) {
	t.Parallel()

	text := buildDingTalkMarkdown(model.LogBatch{
		LogID:      "limit-1",
		FirstSeen:  time.Date(2026, 3, 24, 10, 0, 0, 0, time.UTC),
		LastSeen:   time.Date(2026, 3, 24, 10, 2, 0, 0, time.UTC),
		Count:      3,
		Containers: []string{"local-dev-api"},
		Events: []model.LogEvent{
			{
				Timestamp: time.Date(2026, 3, 24, 10, 0, 0, 0, time.UTC),
				Container: model.ContainerInfo{Name: "local-dev-api"},
				Level:     "ERROR",
				Raw:       "first event",
			},
			{
				Timestamp: time.Date(2026, 3, 24, 10, 1, 0, 0, time.UTC),
				Container: model.ContainerInfo{Name: "local-dev-api"},
				Level:     "ERROR",
				Raw:       "second event",
			},
			{
				Timestamp: time.Date(2026, 3, 24, 10, 2, 0, 0, time.UTC),
				Container: model.ContainerInfo{Name: "local-dev-api"},
				Level:     "ERROR",
				Raw:       "third event",
			},
		},
	}, 2)

	if !strings.Contains(text, "first event") {
		t.Fatalf("markdown text = %q, want first event", text)
	}
	if !strings.Contains(text, "second event") {
		t.Fatalf("markdown text = %q, want second event", text)
	}
	if strings.Contains(text, "third event") {
		t.Fatalf("markdown text = %q, want third event omitted", text)
	}
	if !strings.Contains(text, "其余 `1` 条日志已省略") {
		t.Fatalf("markdown text = %q, want omission summary", text)
	}
}

func TestBuildDingTalkMarkdownFormatsTimesInLocalZone(t *testing.T) {
	t.Parallel()

	utcPlus8 := time.FixedZone("UTC+8", 8*60*60)
	firstSeen := time.Date(2026, 3, 24, 10, 0, 0, 0, time.UTC)
	lastSeen := time.Date(2026, 3, 24, 10, 2, 0, 0, time.UTC)
	eventTime := time.Date(2026, 3, 24, 10, 1, 0, 0, time.UTC)

	text := buildDingTalkMarkdownWithLocation(model.LogBatch{
		LogID:      "tz-1",
		FirstSeen:  firstSeen,
		LastSeen:   lastSeen,
		Count:      1,
		Containers: []string{"local-dev-api"},
		Events: []model.LogEvent{
			{
				Timestamp: eventTime,
				Container: model.ContainerInfo{Name: "local-dev-api"},
				Level:     "WARN",
				Raw:       "event detail",
			},
		},
	}, 5, utcPlus8)

	if !strings.Contains(text, "- 首次时间: `2026-03-24 18:00:00 +08:00`") {
		t.Fatalf("markdown text = %q, want localized first_seen", text)
	}
	if !strings.Contains(text, "- 最后时间: `2026-03-24 18:02:00 +08:00`") {
		t.Fatalf("markdown text = %q, want localized last_seen", text)
	}
	if !strings.Contains(text, "`2026-03-24 18:01:00 +08:00` `local-dev-api` `WARN`") {
		t.Fatalf("markdown text = %q, want localized event timestamp", text)
	}
}

func TestDingTalkStoreAppendBatchHealthErrorMentionsConfiguredUsers(t *testing.T) {
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

	store := NewDingTalkStore(server.URL+"?access_token=test-token", "test-secret", false, []string{"13800000000"}, []string{"ERROR"}, 5)
	err := store.AppendBatch(context.Background(), model.LogBatch{
		LogID:      "monitor.health.docker.event_stream",
		FirstSeen:  time.Date(2026, 4, 1, 10, 0, 0, 0, time.UTC),
		LastSeen:   time.Date(2026, 4, 1, 10, 0, 0, 0, time.UTC),
		Count:      1,
		Containers: []string{"prod-a/monitor"},
		Events: []model.LogEvent{
			{
				Timestamp: time.Date(2026, 4, 1, 10, 0, 0, 0, time.UTC),
				Container: model.ContainerInfo{Name: "prod-a/monitor"},
				Level:     "ERROR",
				Message:   "docker event stream entered degraded state",
				Raw:       "docker event stream entered degraded state: ssh session closed\nconsecutive_failures=3",
			},
		},
	})
	if err != nil {
		t.Fatalf("AppendBatch() error = %v", err)
	}

	if got := len(receivedPayloads); got != 2 {
		t.Fatalf("len(receivedPayloads) = %d, want 2", got)
	}
	if got := receivedPayloads[0].Markdown.Title; got != "Docker监控健康告警 monitor.health.docker.event_stream [prod-a]" {
		t.Fatalf("markdown title = %q, want health title with host", got)
	}
	if !strings.Contains(receivedPayloads[0].Markdown.Text, "- 状态: `ERROR`") {
		t.Fatalf("markdown text = %q, want health status", receivedPayloads[0].Markdown.Text)
	}
	if !strings.Contains(receivedPayloads[0].Markdown.Text, "- 组件: `docker.event_stream`") {
		t.Fatalf("markdown text = %q, want health component", receivedPayloads[0].Markdown.Text)
	}
	if !strings.Contains(receivedPayloads[0].Markdown.Text, "docker event stream entered degraded state") {
		t.Fatalf("markdown text = %q, want health message", receivedPayloads[0].Markdown.Text)
	}
	if !strings.Contains(receivedPayloads[1].Text.Content, "Docker监控健康告警 [prod-a] docker event stream entered degraded state") {
		t.Fatalf("text content = %q, want health mention summary", receivedPayloads[1].Text.Content)
	}
	if !strings.Contains(receivedPayloads[1].Text.Content, "@13800000000") {
		t.Fatalf("text content = %q, want mention", receivedPayloads[1].Text.Content)
	}
}

func TestDingTalkStoreAppendBatchTruncatesOversizedMarkdownPayload(t *testing.T) {
	t.Parallel()

	var (
		requestSizes     []int
		receivedPayloads []dingTalkPayload
	)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("ReadAll() error = %v", err)
		}
		requestSizes = append(requestSizes, len(body))

		var payload dingTalkPayload
		if err := json.Unmarshal(body, &payload); err != nil {
			t.Fatalf("Unmarshal() error = %v", err)
		}
		receivedPayloads = append(receivedPayloads, payload)

		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"errcode":0,"errmsg":"ok"}`))
	}))
	defer server.Close()

	store := NewDingTalkStore(server.URL, "", false, nil, []string{"ERROR"}, 5)
	longLine := strings.Repeat("错误堆栈-", 4000)
	err := store.AppendBatch(context.Background(), model.LogBatch{
		LogID:      "oversized-1",
		FirstSeen:  time.Date(2026, 4, 2, 12, 0, 0, 0, time.UTC),
		LastSeen:   time.Date(2026, 4, 2, 12, 0, 0, 0, time.UTC),
		Count:      1,
		Containers: []string{"coach_test/api"},
		Events: []model.LogEvent{
			{
				Timestamp: time.Date(2026, 4, 2, 12, 0, 0, 0, time.UTC),
				Container: model.ContainerInfo{Name: "coach_test/api"},
				Level:     "ERROR",
				Message:   "request failed",
				Raw:       longLine,
			},
		},
	})
	if err != nil {
		t.Fatalf("AppendBatch() error = %v", err)
	}

	if got := len(requestSizes); got != 1 {
		t.Fatalf("len(requestSizes) = %d, want 1", got)
	}
	if requestSizes[0] > maxDingTalkPayloadBytes {
		t.Fatalf("markdown request size = %d, want <= %d", requestSizes[0], maxDingTalkPayloadBytes)
	}

	markdownPayload := receivedPayloads[0]
	if !strings.Contains(markdownPayload.Markdown.Text, "... (已截断)") {
		t.Fatalf("markdown text = %q, want truncation notice", markdownPayload.Markdown.Text)
	}
	if strings.Contains(markdownPayload.Markdown.Text, longLine) {
		t.Fatalf("markdown text unexpectedly contains full oversized raw log")
	}
}

func TestDingTalkStoreRateLimitsLocalSends(t *testing.T) {
	t.Parallel()

	var requestTimes []time.Time

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestTimes = append(requestTimes, time.Now())
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"errcode":0,"errmsg":"ok"}`))
	}))
	defer server.Close()

	store := newDingTalkStoreWithTiming(server.URL, "", false, nil, []string{"ERROR"}, 5, 70*time.Millisecond, 0)

	first := model.LogBatch{
		LogID:      "rate-limit-1",
		FirstSeen:  time.Date(2026, 4, 2, 12, 0, 0, 0, time.UTC),
		LastSeen:   time.Date(2026, 4, 2, 12, 0, 0, 0, time.UTC),
		Count:      1,
		Containers: []string{"coach_test/api"},
		Events: []model.LogEvent{{
			Timestamp: time.Date(2026, 4, 2, 12, 0, 0, 0, time.UTC),
			Container: model.ContainerInfo{Name: "coach_test/api"},
			Level:     "WARN",
			Message:   "first request",
			Raw:       "first request",
		}},
	}
	second := first
	second.LogID = "rate-limit-2"
	second.Events = []model.LogEvent{{
		Timestamp: time.Date(2026, 4, 2, 12, 0, 1, 0, time.UTC),
		Container: model.ContainerInfo{Name: "coach_test/api"},
		Level:     "WARN",
		Message:   "second request",
		Raw:       "second request",
	}}

	if err := store.AppendBatch(context.Background(), first); err != nil {
		t.Fatalf("first AppendBatch() error = %v", err)
	}
	if err := store.AppendBatch(context.Background(), second); err != nil {
		t.Fatalf("second AppendBatch() error = %v", err)
	}

	if got := len(requestTimes); got != 2 {
		t.Fatalf("len(requestTimes) = %d, want 2", got)
	}
	if delta := requestTimes[1].Sub(requestTimes[0]); delta < 45*time.Millisecond {
		t.Fatalf("request spacing = %s, want at least %s", delta, 45*time.Millisecond)
	}
}

func TestDingTalkStoreAppliesCooldownAfterRateLimitResponse(t *testing.T) {
	t.Parallel()

	var requestTimes []time.Time
	requestCount := 0

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestTimes = append(requestTimes, time.Now())
		w.Header().Set("Content-Type", "application/json")
		if requestCount == 0 {
			_, _ = w.Write([]byte(`{"errcode":660026,"errmsg":"sending too many messages per minute"}`))
			requestCount++
			return
		}
		requestCount++
		_, _ = w.Write([]byte(`{"errcode":0,"errmsg":"ok"}`))
	}))
	defer server.Close()

	store := newDingTalkStoreWithTiming(server.URL, "", false, nil, []string{"ERROR"}, 5, 0, 50*time.Millisecond)

	batch := model.LogBatch{
		LogID:      "cooldown-1",
		FirstSeen:  time.Date(2026, 4, 2, 12, 0, 0, 0, time.UTC),
		LastSeen:   time.Date(2026, 4, 2, 12, 0, 0, 0, time.UTC),
		Count:      1,
		Containers: []string{"coach_test/api"},
		Events: []model.LogEvent{{
			Timestamp: time.Date(2026, 4, 2, 12, 0, 0, 0, time.UTC),
			Container: model.ContainerInfo{Name: "coach_test/api"},
			Level:     "WARN",
			Message:   "first request",
			Raw:       "first request",
		}},
	}

	err := store.AppendBatch(context.Background(), batch)
	if err == nil {
		t.Fatal("first AppendBatch() error = nil, want rate limit error")
	}
	if !strings.Contains(err.Error(), "660026") {
		t.Fatalf("first AppendBatch() error = %v, want rate limit code", err)
	}
	if !strings.Contains(err.Error(), "local cooldown") {
		t.Fatalf("first AppendBatch() error = %v, want cooldown hint", err)
	}

	batch.LogID = "cooldown-2"
	if err := store.AppendBatch(context.Background(), batch); err != nil {
		t.Fatalf("second AppendBatch() error = %v", err)
	}

	if got := len(requestTimes); got != 2 {
		t.Fatalf("len(requestTimes) = %d, want 2", got)
	}
	if delta := requestTimes[1].Sub(requestTimes[0]); delta < 30*time.Millisecond {
		t.Fatalf("request spacing after rate limit = %s, want at least %s", delta, 30*time.Millisecond)
	}
}

func TestDingTalkStoreSkipsBatchWhenSuppressAlertSinks(t *testing.T) {
	t.Parallel()

	var requests int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests++
		_, _ = w.Write([]byte(`{"errcode":0,"errmsg":"ok"}`))
	}))
	defer server.Close()

	store := NewDingTalkStore(server.URL, "", false, nil, []string{"WARN", "ERROR"}, 5)

	batch := model.LogBatch{
		LogID:              "suppress-1",
		FirstSeen:          time.Date(2026, 4, 17, 9, 0, 0, 0, time.UTC),
		LastSeen:           time.Date(2026, 4, 17, 9, 1, 0, 0, time.UTC),
		Count:              1,
		Containers:         []string{"coach_prod/api"},
		SuppressAlertSinks: true,
		Events: []model.LogEvent{{
			Timestamp:    time.Date(2026, 4, 17, 9, 0, 0, 0, time.UTC),
			Container:    model.ContainerInfo{Name: "coach_prod/api"},
			Level:        "WARN",
			Message:      "replayed historical alert",
			Raw:          "replayed historical alert",
			AlertMatched: true,
		}},
	}

	if err := store.AppendBatch(context.Background(), batch); err != nil {
		t.Fatalf("AppendBatch() error = %v", err)
	}
	if requests != 0 {
		t.Fatalf("dingtalk received %d requests, want 0 because batch was marked SuppressAlertSinks", requests)
	}
}
