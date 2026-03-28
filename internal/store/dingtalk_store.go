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
	"time"

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
}

const maxDingTalkResponseBytes = 1 << 20

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
	}
}

func (s *DingTalkStore) AppendBatch(ctx context.Context, batch model.LogBatch) error {
	if s == nil {
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
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshal dingtalk payload: %w", err)
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
		return fmt.Errorf("dingtalk send failed: %s (%d)", result.ErrMsg, result.ErrCode)
	}

	return nil
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

func formatDisplayTime(ts time.Time) string {
	return formatDisplayTimeInLocation(ts, time.Local)
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
	title := "Docker日志告警 " + batch.LogID
	if hosts := extractDockerHosts(batch); len(hosts) > 0 {
		title += " [" + strings.Join(hosts, ",") + "]"
	}
	return title
}

func buildDingTalkMentionText(batch model.LogBatch, atAll bool, atMobiles []string) string {
	var builder strings.Builder
	builder.WriteString(buildDingTalkTitle(batch))

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
