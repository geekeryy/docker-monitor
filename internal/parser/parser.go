package parser

import (
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/geekeryy/docker-monitor/internal/config"
	"github.com/geekeryy/docker-monitor/internal/model"
)

// chainBlockerMaxEntries 限制链路过滤缓存上限，防止极端情况下内存爆炸。
// 触达上限时会清理过期项，必要时按过期时间最早的批量淘汰。
const chainBlockerMaxEntries = 100_000

type Parser struct {
	warnFields     []string
	messageFields  []string
	timeFields     []string
	warnRegexps    []*regexp.Regexp
	excludeRegexps []*regexp.Regexp
	logIDKeys      []string
	logIDRegexps   []*regexp.Regexp
	unknownLogID   string
	chainBlocker   *chainBlocker
	now            func() time.Time
}

// SetNowFunc 替换 Parser 内部使用的时间源，仅用于测试。
// 注意：和 Parse 并发调用不安全，请在 Parser 投入生产前完成注入。
func (p *Parser) SetNowFunc(fn func() time.Time) {
	if fn == nil {
		return
	}
	p.now = fn
}

// New 构造 Parser。chainTTL > 0 表示开启链路级过滤：被 exclude
// 命中的 log_id 会在 TTL 内将同 log_id 的后续日志全部静默。
func New(cfg config.FilterConfig, unknownLogID string) (*Parser, error) {
	warnRegexps := make([]*regexp.Regexp, 0, len(cfg.WarnMatch.Regexps))
	for _, expr := range cfg.WarnMatch.Regexps {
		re, err := regexp.Compile(expr)
		if err != nil {
			return nil, fmt.Errorf("compile warn regexp %q: %w", expr, err)
		}
		warnRegexps = append(warnRegexps, re)
	}

	regexps := make([]*regexp.Regexp, 0, len(cfg.LogIDExtract.Regexps))
	for _, expr := range cfg.LogIDExtract.Regexps {
		re, err := regexp.Compile(expr)
		if err != nil {
			return nil, fmt.Errorf("compile log id regexp %q: %w", expr, err)
		}
		regexps = append(regexps, re)
	}

	excludeRegexps := make([]*regexp.Regexp, 0, len(cfg.WarnMatch.ExcludeRegexps)+1)
	for _, expr := range cfg.WarnMatch.ExcludeRegexps {
		re, err := regexp.Compile(expr)
		if err != nil {
			return nil, fmt.Errorf("compile warn exclude regexp %q: %w", expr, err)
		}
		excludeRegexps = append(excludeRegexps, re)
	}

	if re, err := buildExcludeIPRegexp(cfg.WarnMatch.ExcludeIPs); err != nil {
		return nil, err
	} else if re != nil {
		excludeRegexps = append(excludeRegexps, re)
	}

	chainTTL, err := parseChainTTL(cfg.WarnMatch.ExcludeChainTTL)
	if err != nil {
		return nil, err
	}

	p := &Parser{
		warnFields:     normalizeKeys(cfg.WarnMatch.JSONFields),
		messageFields:  normalizeKeys(cfg.WarnMatch.MessageFields),
		timeFields:     normalizeKeys(cfg.WarnMatch.TimeFields),
		warnRegexps:    warnRegexps,
		excludeRegexps: excludeRegexps,
		logIDKeys:      normalizeKeys(cfg.LogIDExtract.JSONKeys),
		logIDRegexps:   regexps,
		unknownLogID:   unknownLogID,
		chainBlocker:   newChainBlocker(chainTTL, chainBlockerMaxEntries),
		now:            func() time.Time { return time.Now().UTC() },
	}

	if len(p.warnRegexps) == 0 {
		p.warnRegexps = []*regexp.Regexp{regexp.MustCompile(`(?i)\bWARN\b`)}
	}
	if len(p.messageFields) == 0 {
		p.messageFields = []string{"message", "msg", "log"}
	}
	if len(p.timeFields) == 0 {
		p.timeFields = []string{"time", "timestamp", "@timestamp", "ts"}
	}

	return p, nil
}

func (p *Parser) Parse(raw model.RawLog) (*model.LogEvent, bool, error) {
	line := strings.TrimSpace(raw.Line)
	if line == "" {
		return nil, false, nil
	}

	event := &model.LogEvent{
		Timestamp: raw.Timestamp,
		Container: raw.Container,
		Stream:    raw.Stream,
		Message:   line,
		Raw:       line,
		LogID:     p.unknownLogID,
	}

	parsedJSON := map[string]any{}
	jsonOK := false
	if err := json.Unmarshal([]byte(line), &parsedJSON); err == nil {
		jsonOK = true
		event.ParsedFromJSON = true
		event.Message = coalesceString(parsedJSON, p.messageFields, line)
		if ts, ok := parseTimestamp(parsedJSON, p.timeFields); ok {
			event.Timestamp = ts
		}
		if level, ok := extractWarnLevel(parsedJSON, p.warnFields, p.warnRegexps); ok {
			event.Level = level
			event.AlertMatched = true
		}
		if logID, ok := extractLogIDFromJSON(parsedJSON, p.logIDKeys); ok {
			event.LogID = logID
		}
	}

	if event.Level == "" {
		level, ok := extractWarnLevelFromText(line, p.warnRegexps)
		if ok {
			event.Level = level
			event.AlertMatched = true
		}
	}

	// 把文本 log_id 提取提前到 exclude 判断之前。
	// 原因：被 exclude 命中的日志也需要拿到 log_id 用于登记链路，
	// 否则后续不含 IP 但同 log_id 的日志会漏过滤。
	if event.LogID == p.unknownLogID {
		if logID, ok := extractLogIDFromText(event.Message, p.logIDRegexps); ok {
			event.LogID = logID
		} else if logID, ok := extractLogIDFromText(line, p.logIDRegexps); ok {
			event.LogID = logID
		}
	}

	excluded := hasAnyRegexp(event.Message, p.excludeRegexps) ||
		hasAnyRegexp(line, p.excludeRegexps) ||
		(jsonOK && payloadHasAnyRegexp(parsedJSON, p.excludeRegexps))

	now := p.now()
	if excluded {
		if event.LogID != p.unknownLogID {
			p.chainBlocker.Block(event.LogID, now)
		}
		return nil, false, nil
	}
	if event.LogID != p.unknownLogID && p.chainBlocker.Blocked(event.LogID, now) {
		return nil, false, nil
	}

	if !event.AlertMatched && event.LogID == p.unknownLogID {
		return nil, false, nil
	}

	return event, true, nil
}

func normalizeKeys(values []string) []string {
	out := make([]string, 0, len(values))
	for _, value := range values {
		if trimmed := strings.TrimSpace(strings.ToLower(value)); trimmed != "" {
			out = append(out, trimmed)
		}
	}
	return out
}

func coalesceString(payload map[string]any, keys []string, fallback string) string {
	for _, key := range keys {
		for field, value := range payload {
			if strings.EqualFold(field, key) {
				switch typed := value.(type) {
				case string:
					if typed != "" {
						return typed
					}
				default:
					text := strings.TrimSpace(fmt.Sprint(typed))
					if text != "" {
						return text
					}
				}
			}
		}
	}
	return fallback
}

func parseTimestamp(payload map[string]any, keys []string) (time.Time, bool) {
	for _, key := range keys {
		for field, value := range payload {
			if !strings.EqualFold(field, key) {
				continue
			}

			switch typed := value.(type) {
			case string:
				if ts, ok := parseTimestampString(typed); ok {
					return ts, true
				}
			case float64:
				return time.Unix(int64(typed), 0).UTC(), true
			case int64:
				return time.Unix(typed, 0).UTC(), true
			}
		}
	}
	return time.Time{}, false
}

func parseTimestampString(value string) (time.Time, bool) {
	if ts, err := time.Parse(time.RFC3339Nano, value); err == nil {
		return ts, true
	}
	if ts, err := time.Parse(time.RFC3339, value); err == nil {
		return ts, true
	}
	if ts, err := time.ParseInLocation("2006-01-02 15:04:05", value, time.Local); err == nil {
		return ts, true
	}
	if ts, err := time.ParseInLocation("2006-01-02 15:04:05.000", value, time.Local); err == nil {
		return ts, true
	}
	if unixSeconds, err := strconv.ParseInt(value, 10, 64); err == nil {
		return time.Unix(unixSeconds, 0).UTC(), true
	}
	return time.Time{}, false
}

func extractWarnLevel(payload map[string]any, fields []string, regexps []*regexp.Regexp) (string, bool) {
	for _, wanted := range fields {
		for field, value := range payload {
			if !strings.EqualFold(field, wanted) {
				continue
			}
			level := strings.TrimSpace(fmt.Sprint(value))
			if matched, ok := matchWarnRegexp(level, regexps); ok {
				return matched, true
			}
		}
	}
	return "", false
}

func extractWarnLevelFromText(line string, regexps []*regexp.Regexp) (string, bool) {
	return matchWarnRegexp(line, regexps)
}

func matchWarnRegexp(text string, regexps []*regexp.Regexp) (string, bool) {
	for _, re := range regexps {
		matches := re.FindStringSubmatch(text)
		if len(matches) == 0 {
			continue
		}
		for _, match := range matches[1:] {
			if trimmed := strings.TrimSpace(match); trimmed != "" {
				return strings.ToUpper(trimmed), true
			}
		}
		if trimmed := strings.TrimSpace(matches[0]); trimmed != "" {
			return strings.ToUpper(trimmed), true
		}
	}
	return "", false
}

func hasAnyRegexp(text string, regexps []*regexp.Regexp) bool {
	for _, re := range regexps {
		if re.MatchString(text) {
			return true
		}
	}
	return false
}

func payloadHasAnyRegexp(value any, regexps []*regexp.Regexp) bool {
	switch typed := value.(type) {
	case map[string]any:
		for _, item := range typed {
			if payloadHasAnyRegexp(item, regexps) {
				return true
			}
		}
	case []any:
		for _, item := range typed {
			if payloadHasAnyRegexp(item, regexps) {
				return true
			}
		}
	case string:
		return hasAnyRegexp(typed, regexps)
	case nil:
		return false
	default:
		text := strings.TrimSpace(fmt.Sprint(typed))
		if text == "" {
			return false
		}
		return hasAnyRegexp(text, regexps)
	}

	return false
}

func extractLogIDFromJSON(payload map[string]any, keys []string) (string, bool) {
	for _, wanted := range keys {
		for field, value := range payload {
			if !strings.EqualFold(field, wanted) {
				continue
			}
			text := strings.TrimSpace(fmt.Sprint(value))
			if text != "" {
				return text, true
			}
		}
	}
	return "", false
}

func extractLogIDFromText(line string, regexps []*regexp.Regexp) (string, bool) {
	for _, re := range regexps {
		matches := re.FindStringSubmatch(line)
		if len(matches) > 1 && strings.TrimSpace(matches[1]) != "" {
			return strings.TrimSpace(matches[1]), true
		}
	}
	return "", false
}

// buildExcludeIPRegexp 把 IP 白名单合并为一条带词边界的正则。
//
// 每条 IP 都会经过 regexp.QuoteMeta，避免点号被当作正则元字符；
// 同时整体加上 \b...\b，避免 "1.1.1.10" 被 "1.1.1.1" 误命中。
// 返回 nil 表示没有有效 IP（空列表或仅含空白），调用方按未配置处理。
func buildExcludeIPRegexp(ips []string) (*regexp.Regexp, error) {
	quoted := make([]string, 0, len(ips))
	for _, ip := range ips {
		ip = strings.TrimSpace(ip)
		if ip == "" {
			continue
		}
		quoted = append(quoted, regexp.QuoteMeta(ip))
	}
	if len(quoted) == 0 {
		return nil, nil
	}
	pattern := `\b(?:` + strings.Join(quoted, "|") + `)\b`
	re, err := regexp.Compile(pattern)
	if err != nil {
		return nil, fmt.Errorf("compile warn exclude ips %q: %w", pattern, err)
	}
	return re, nil
}

func parseChainTTL(raw string) (time.Duration, error) {
	value := strings.TrimSpace(raw)
	if value == "" {
		return 0, nil
	}
	d, err := time.ParseDuration(value)
	if err != nil {
		return 0, fmt.Errorf("parse exclude_chain_ttl %q: %w", value, err)
	}
	if d < 0 {
		return 0, fmt.Errorf("exclude_chain_ttl must be >= 0, got %s", d)
	}
	return d, nil
}

// chainBlocker 缓存被 exclude 命中的 log_id，在 TTL 内静默后续同 log_id 的日志。
//
// 设计要点：
//   - ttl <= 0 时由 newChainBlocker 返回 nil，Block/Blocked 走 nil 安全分支，
//     实现"零开销关闭"，外部无需做开关判断。
//   - 内存上限由 maxEntries 兜底；写入触达上限时惰性清理过期项，
//     仍超出则按过期时间最早的批量淘汰，避免在线时长拉满后无限增长。
//   - 用 sync.Mutex 保护，读写频次相近，没必要引入 RWMutex 复杂度。
type chainBlocker struct {
	ttl        time.Duration
	maxEntries int
	mu         sync.Mutex
	entries    map[string]time.Time
}

func newChainBlocker(ttl time.Duration, maxEntries int) *chainBlocker {
	if ttl <= 0 {
		return nil
	}
	if maxEntries <= 0 {
		maxEntries = chainBlockerMaxEntries
	}
	return &chainBlocker{
		ttl:        ttl,
		maxEntries: maxEntries,
		entries:    make(map[string]time.Time),
	}
}

// Block 登记一条被 exclude 命中的链路。logID 为空时直接忽略。
func (b *chainBlocker) Block(logID string, now time.Time) {
	if b == nil || logID == "" {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.entries[logID] = now.Add(b.ttl)
	if len(b.entries) > b.maxEntries {
		b.evictLocked(now)
	}
}

// Blocked 查询某 log_id 当前是否处在静默期。命中过期项会顺手清理。
func (b *chainBlocker) Blocked(logID string, now time.Time) bool {
	if b == nil || logID == "" {
		return false
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	expireAt, ok := b.entries[logID]
	if !ok {
		return false
	}
	if !now.Before(expireAt) {
		delete(b.entries, logID)
		return false
	}
	return true
}

func (b *chainBlocker) evictLocked(now time.Time) {
	for id, expireAt := range b.entries {
		if !now.Before(expireAt) {
			delete(b.entries, id)
		}
	}
	if len(b.entries) <= b.maxEntries {
		return
	}
	type kv struct {
		id  string
		exp time.Time
	}
	items := make([]kv, 0, len(b.entries))
	for id, exp := range b.entries {
		items = append(items, kv{id, exp})
	}
	sort.Slice(items, func(i, j int) bool { return items[i].exp.Before(items[j].exp) })
	target := b.maxEntries * 8 / 10
	for i := 0; i < len(items) && len(b.entries) > target; i++ {
		delete(b.entries, items[i].id)
	}
}
