package store

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/geekeryy/docker-monitor/internal/model"
)

type DingTalkStore struct {
	webhookURL    string
	secret        string
	atAll         bool
	atMobiles     []string
	mentionLevels map[string]struct{}
	maxEvents     int
	client        *http.Client

	// sendMu 把 waitForSendWindow 序列化，保证多并发调用之间的
	// "下次允许发送时间"语义不会互相覆盖。它独立于 mu：mu 只保护
	// nextSendAt / blockedUntil 等小字段，sendMu 则覆盖整个等待 + 推进流程。
	sendMu           sync.Mutex
	mu               sync.Mutex
	sendInterval     time.Duration
	rateLimitBackoff time.Duration
	nextSendAt       time.Time
	blockedUntil     time.Time
}

const maxDingTalkResponseBytes = 1 << 20
const maxDingTalkPayloadBytes = 20000
const dingTalkPayloadTruncationNotice = "\n\n... (已截断)"
const defaultDingTalkSendInterval = 4 * time.Second
const defaultDingTalkRateLimitBackoff = time.Minute
const dingTalkErrCodeRateLimit = 660026

type dingTalkPayload struct {
	MsgType  string               `json:"msgtype"`
	Markdown dingTalkMarkdownBody `json:"markdown,omitempty"`
	Text     dingTalkTextBody     `json:"text,omitempty"`
	At       dingTalkAtBody       `json:"at,omitempty"`
}

type dingTalkMarkdownBody struct {
	Title string `json:"title"`
	Text  string `json:"text"`
}

type dingTalkTextBody struct {
	Content string `json:"content"`
}

type dingTalkAtBody struct {
	AtMobiles []string `json:"atMobiles"`
	IsAtAll   bool     `json:"isAtAll"`
}

type dingTalkResponse struct {
	ErrCode int    `json:"errcode"`
	ErrMsg  string `json:"errmsg"`
}

func NewDingTalkStore(webhookURL, secret string, atAll bool, atMobiles []string, mentionLevels []string, maxEvents int) *DingTalkStore {
	return newDingTalkStoreWithTiming(
		webhookURL,
		secret,
		atAll,
		atMobiles,
		mentionLevels,
		maxEvents,
		defaultDingTalkSendInterval,
		defaultDingTalkRateLimitBackoff,
	)
}

func newDingTalkStoreWithTiming(webhookURL, secret string, atAll bool, atMobiles []string, mentionLevels []string, maxEvents int, sendInterval, rateLimitBackoff time.Duration) *DingTalkStore {
	if strings.TrimSpace(webhookURL) == "" {
		return nil
	}
	if maxEvents <= 0 {
		maxEvents = 5
	}
	return &DingTalkStore{
		webhookURL:    strings.TrimSpace(webhookURL),
		secret:        strings.TrimSpace(secret),
		atAll:         atAll,
		atMobiles:     atMobiles,
		mentionLevels: normalizeLevels(mentionLevels),
		maxEvents:     maxEvents,
		client: &http.Client{
			Timeout: 8 * time.Second,
		},
		sendInterval:     sendInterval,
		rateLimitBackoff: rateLimitBackoff,
	}
}

func (s *DingTalkStore) AppendBatch(ctx context.Context, batch model.LogBatch) error {
	if s == nil {
		return nil
	}
	if batch.SuppressAlertSinks {
		return nil
	}

	shouldMention := batchHasAnyLevel(batch, s.mentionLevels)
	atMobiles := mentionedMobiles(s.atMobiles, shouldMention)
	markdownPayload := dingTalkPayload{
		MsgType: "markdown",
		Markdown: dingTalkMarkdownBody{
			Title: buildDingTalkTitle(batch),
			Text:  buildDingTalkMarkdown(batch, s.maxEvents),
		},
	}

	if err := s.sendPayload(ctx, markdownPayload); err != nil {
		return err
	}

	if !shouldMention {
		return nil
	}

	mentionPayload := dingTalkPayload{
		MsgType: "text",
		Text: dingTalkTextBody{
			Content: buildDingTalkMentionText(batch, s.atAll, atMobiles),
		},
		At: dingTalkAtBody{
			AtMobiles: atMobiles,
			IsAtAll:   s.atAll,
		},
	}
	if mentionPayload.At.IsAtAll || len(mentionPayload.At.AtMobiles) > 0 {
		if err := s.sendPayload(ctx, mentionPayload); err != nil {
			return err
		}
	}

	return nil
}

func (s *DingTalkStore) sendPayload(ctx context.Context, payload dingTalkPayload) error {
	if err := s.waitForSendWindow(ctx); err != nil {
		return err
	}

	body, err := marshalDingTalkPayload(payload)
	if err != nil {
		return err
	}

	reqURL, err := signedWebhookURL(s.webhookURL, s.secret)
	if err != nil {
		return err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, reqURL, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("create dingtalk request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := s.client.Do(req)
	if err != nil {
		return fmt.Errorf("send dingtalk request: %w", err)
	}
	defer closeSilently(resp.Body)

	var result dingTalkResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxDingTalkResponseBytes)).Decode(&result); err != nil {
		return fmt.Errorf("decode dingtalk response: %w", err)
	}
	if resp.StatusCode >= 300 {
		return fmt.Errorf("dingtalk http status %s: %s", resp.Status, result.ErrMsg)
	}
	if result.ErrCode != 0 {
		if result.ErrCode == dingTalkErrCodeRateLimit {
			s.noteRateLimit()
			return fmt.Errorf("dingtalk send failed: %s (%d), local cooldown %s applied", result.ErrMsg, result.ErrCode, s.rateLimitBackoff)
		}
		return fmt.Errorf("dingtalk send failed: %s (%d)", result.ErrMsg, result.ErrCode)
	}

	return nil
}

func (s *DingTalkStore) waitForSendWindow(ctx context.Context) error {
	if s == nil {
		return nil
	}

	// sendMu 串行所有发送线程，确保 sendInterval 是一个真正排队的窗口；
	// 同时也避免"两个调用都看到 nextSendAt 已过期 -> 同时发送"的竞争。
	s.sendMu.Lock()
	defer s.sendMu.Unlock()

	s.mu.Lock()
	waitUntil := s.nextSendAt
	if s.blockedUntil.After(waitUntil) {
		waitUntil = s.blockedUntil
	}
	s.mu.Unlock()

	if delay := time.Until(waitUntil); delay > 0 {
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			// 等待被取消，没有真正发送，nextSendAt 不推进，
			// 让下一次调用能立即占用这个窗口。
			return ctx.Err()
		case <-timer.C:
		}
	}

	if s.sendInterval > 0 {
		s.mu.Lock()
		s.nextSendAt = time.Now().Add(s.sendInterval)
		s.mu.Unlock()
	}
	return nil
}

func (s *DingTalkStore) noteRateLimit() {
	if s == nil || s.rateLimitBackoff <= 0 {
		return
	}

	blockedUntil := time.Now().Add(s.rateLimitBackoff)

	s.mu.Lock()
	if blockedUntil.After(s.blockedUntil) {
		s.blockedUntil = blockedUntil
	}
	s.mu.Unlock()
}

func marshalDingTalkPayload(payload dingTalkPayload) ([]byte, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("marshal dingtalk payload: %w", err)
	}
	if len(body) <= maxDingTalkPayloadBytes {
		return body, nil
	}

	trimmedPayload, ok, err := truncateDingTalkPayload(payload)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, fmt.Errorf("dingtalk payload exceeds %d bytes", maxDingTalkPayloadBytes)
	}

	body, err = json.Marshal(trimmedPayload)
	if err != nil {
		return nil, fmt.Errorf("marshal truncated dingtalk payload: %w", err)
	}
	if len(body) > maxDingTalkPayloadBytes {
		return nil, fmt.Errorf("dingtalk payload exceeds %d bytes after truncation", maxDingTalkPayloadBytes)
	}
	return body, nil
}

func truncateDingTalkPayload(payload dingTalkPayload) (dingTalkPayload, bool, error) {
	switch payload.MsgType {
	case "markdown":
		text, ok, err := truncatePayloadString(payload.Markdown.Text, func(content string) dingTalkPayload {
			trimmed := payload
			trimmed.Markdown.Text = content
			return trimmed
		})
		if err != nil || !ok {
			return dingTalkPayload{}, ok, err
		}
		payload.Markdown.Text = text
		return payload, true, nil
	case "text":
		text, ok, err := truncatePayloadString(payload.Text.Content, func(content string) dingTalkPayload {
			trimmed := payload
			trimmed.Text.Content = content
			return trimmed
		})
		if err != nil || !ok {
			return dingTalkPayload{}, ok, err
		}
		payload.Text.Content = text
		return payload, true, nil
	default:
		return dingTalkPayload{}, false, nil
	}
}

func truncatePayloadString(content string, build func(string) dingTalkPayload) (string, bool, error) {
	candidate := strings.TrimRight(content, "\n")
	best := dingTalkPayloadTruncationNotice

	body, err := json.Marshal(build(best))
	if err != nil {
		return "", false, fmt.Errorf("marshal truncated dingtalk payload: %w", err)
	}
	if len(body) > maxDingTalkPayloadBytes {
		return "", false, nil
	}

	low, high := 0, len(candidate)
	for low <= high {
		mid := (low + high) / 2
		truncated := strings.TrimRight(truncateUTF8ByBytes(candidate, mid), "\n") + dingTalkPayloadTruncationNotice

		body, err := json.Marshal(build(truncated))
		if err != nil {
			return "", false, fmt.Errorf("marshal truncated dingtalk payload: %w", err)
		}
		if len(body) <= maxDingTalkPayloadBytes {
			best = truncated
			low = mid + 1
			continue
		}
		high = mid - 1
	}

	return best, true, nil
}

func truncateUTF8ByBytes(text string, limit int) string {
	if limit <= 0 {
		return ""
	}
	if len(text) <= limit {
		return text
	}
	for limit > 0 && limit < len(text) && !utf8.RuneStart(text[limit]) {
		limit--
	}
	return text[:limit]
}

func signedWebhookURL(rawURL, secret string) (string, error) {
	if secret == "" {
		return rawURL, nil
	}

	timestamp := fmt.Sprintf("%d", time.Now().UnixMilli())
	stringToSign := timestamp + "\n" + secret
	mac := hmac.New(sha256.New, []byte(secret))
	if _, err := mac.Write([]byte(stringToSign)); err != nil {
		return "", fmt.Errorf("sign dingtalk webhook: %w", err)
	}

	signature := url.QueryEscape(base64.StdEncoding.EncodeToString(mac.Sum(nil)))
	separator := "?"
	if strings.Contains(rawURL, "?") {
		separator = "&"
	}
	return fmt.Sprintf("%s%stimestamp=%s&sign=%s", rawURL, separator, timestamp, signature), nil
}

func buildDingTalkMarkdown(batch model.LogBatch, maxEvents int) string {
	return buildDingTalkMarkdownWithLocation(batch, maxEvents, time.Local)
}

func buildDingTalkMarkdownWithLocation(batch model.LogBatch, maxEvents int, location *time.Location) string {
	if isHealthBatch(batch) {
		return buildHealthDingTalkMarkdown(batch, location)
	}

	var builder strings.Builder
	builder.WriteString("### ")
	builder.WriteString(buildDingTalkTitle(batch))
	builder.WriteString("\n\n")
	if hosts := extractDockerHosts(batch); len(hosts) > 0 {
		builder.WriteString(fmt.Sprintf("- 主机: `%s`\n", strings.Join(hosts, ", ")))
	}
	builder.WriteString(fmt.Sprintf("- LogID: `%s`\n", batch.LogID))
	builder.WriteString(fmt.Sprintf("- 日志条数: `%d`\n", batch.Count))
	builder.WriteString(fmt.Sprintf("- 容器: `%s`\n", strings.Join(batch.Containers, ", ")))
	builder.WriteString(fmt.Sprintf("- 首次时间: `%s`\n", formatDisplayTimeInLocation(batch.FirstSeen, location)))
	builder.WriteString(fmt.Sprintf("- 最后时间: `%s`\n", formatDisplayTimeInLocation(batch.LastSeen, location)))
	builder.WriteString("\n#### 最近日志\n")

	if maxEvents <= 0 {
		maxEvents = 5
	}
	for i, event := range batch.Events {
		if i >= maxEvents {
			builder.WriteString(fmt.Sprintf("\n- 其余 `%d` 条日志已省略", len(batch.Events)-maxEvents))
			break
		}
		builder.WriteString(fmt.Sprintf("\n- `%s` `%s` `%s`",
			formatDisplayTimeInLocation(event.Timestamp, location),
			event.Container.Name,
			emptyAsDash(event.Level),
		))
		builder.WriteString("\n\n```\n")
		builder.WriteString(detailedLog(event))
		builder.WriteString("\n```\n")
	}

	return builder.String()
}

func buildHealthDingTalkMarkdown(batch model.LogBatch, location *time.Location) string {
	var builder strings.Builder
	event := firstBatchEvent(batch)

	builder.WriteString("### ")
	builder.WriteString(buildDingTalkTitle(batch))
	builder.WriteString("\n\n")
	if hosts := extractDockerHosts(batch); len(hosts) > 0 {
		builder.WriteString(fmt.Sprintf("- 主机: `%s`\n", strings.Join(hosts, ", ")))
	}
	builder.WriteString(fmt.Sprintf("- 状态: `%s`\n", emptyAsDash(event.Level)))
	builder.WriteString(fmt.Sprintf("- 组件: `%s`\n", healthComponent(batch.LogID)))
	builder.WriteString(fmt.Sprintf("- 时间: `%s`\n", formatDisplayTimeInLocation(event.Timestamp, location)))
	builder.WriteString(fmt.Sprintf("- 容器: `%s`\n", strings.Join(batch.Containers, ", ")))
	builder.WriteString("\n#### 说明\n\n")
	builder.WriteString(strings.TrimSpace(event.Message))
	builder.WriteString("\n\n#### 详情\n\n```\n")
	builder.WriteString(detailedLog(event))
	builder.WriteString("\n```\n")

	return builder.String()
}

func formatDisplayTimeInLocation(ts time.Time, location *time.Location) string {
	if ts.IsZero() {
		return "-"
	}
	if location == nil {
		location = time.Local
	}
	return ts.In(location).Format("2006-01-02 15:04:05 -07:00")
}

func detailedLog(event model.LogEvent) string {
	text := strings.TrimSpace(event.Raw)
	if text == "" {
		text = strings.TrimSpace(event.Message)
	}
	if text == "" {
		return "-"
	}

	text = strings.ReplaceAll(text, "```", "'''")

	const maxDetailLength = 5000
	if len(text) > maxDetailLength {
		return text[:maxDetailLength] + "\n... (已截断)"
	}

	return text
}

func batchHasAnyLevel(batch model.LogBatch, targets map[string]struct{}) bool {
	if len(targets) == 0 {
		return false
	}
	for _, event := range batch.Events {
		if _, ok := targets[strings.TrimSpace(strings.ToUpper(event.Level))]; ok {
			return true
		}
	}
	return false
}

func normalizeLevels(levels []string) map[string]struct{} {
	normalized := make(map[string]struct{}, len(levels))
	for _, level := range levels {
		level = strings.TrimSpace(strings.ToUpper(level))
		if level == "" {
			continue
		}
		normalized[level] = struct{}{}
	}
	return normalized
}

func mentionedMobiles(mobiles []string, shouldMention bool) []string {
	if !shouldMention || len(mobiles) == 0 {
		return nil
	}
	return mobiles
}

func renderMobileMentions(mobiles []string) string {
	var builder strings.Builder
	for i, mobile := range mobiles {
		mobile = strings.TrimSpace(mobile)
		if mobile == "" {
			continue
		}
		if i > 0 {
			builder.WriteString(" ")
		}
		builder.WriteString("@")
		builder.WriteString(mobile)
	}
	return builder.String()
}

func buildDingTalkTitle(batch model.LogBatch) string {
	titlePrefix := "Docker日志告警 "
	if isHealthBatch(batch) {
		titlePrefix = "Docker监控健康告警 "
	}
	title := titlePrefix + batch.LogID
	if hosts := extractDockerHosts(batch); len(hosts) > 0 {
		title += " [" + strings.Join(hosts, ",") + "]"
	}
	return title
}

func buildDingTalkMentionText(batch model.LogBatch, atAll bool, atMobiles []string) string {
	var builder strings.Builder
	if isHealthBatch(batch) {
		builder.WriteString("Docker监控健康告警")
		if hosts := extractDockerHosts(batch); len(hosts) > 0 {
			builder.WriteString(" [")
			builder.WriteString(strings.Join(hosts, ","))
			builder.WriteString("]")
		}
		event := firstBatchEvent(batch)
		if strings.TrimSpace(event.Message) != "" {
			builder.WriteString(" ")
			builder.WriteString(strings.TrimSpace(event.Message))
		}
	} else {
		builder.WriteString(buildDingTalkTitle(batch))
	}

	if atAll {
		builder.WriteString(" @all")
		return builder.String()
	}

	mentions := renderMobileMentions(atMobiles)
	if mentions != "" {
		builder.WriteString(" ")
		builder.WriteString(mentions)
	}
	return builder.String()
}

func isHealthBatch(batch model.LogBatch) bool {
	return strings.HasPrefix(strings.TrimSpace(batch.LogID), "monitor.health.")
}

func healthComponent(logID string) string {
	return strings.TrimPrefix(strings.TrimSpace(logID), "monitor.health.")
}

func firstBatchEvent(batch model.LogBatch) model.LogEvent {
	if len(batch.Events) == 0 {
		return model.LogEvent{}
	}
	return batch.Events[0]
}

func extractDockerHosts(batch model.LogBatch) []string {
	seen := make(map[string]struct{})
	hosts := make([]string, 0, len(batch.Containers))
	for _, container := range batch.Containers {
		idx := strings.Index(container, "/")
		if idx <= 0 {
			continue
		}
		host := strings.TrimSpace(container[:idx])
		if host == "" {
			continue
		}
		if _, exists := seen[host]; exists {
			continue
		}
		seen[host] = struct{}{}
		hosts = append(hosts, host)
	}
	return hosts
}

func closeSilently(closer io.Closer) {
	if closer == nil {
		return
	}
	_ = closer.Close()
}

func emptyAsDash(value string) string {
	if strings.TrimSpace(value) == "" {
		return "-"
	}
	return value
}
