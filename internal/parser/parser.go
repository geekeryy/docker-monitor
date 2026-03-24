package parser

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/geekeryy/docker-monitor/internal/config"
	"github.com/geekeryy/docker-monitor/internal/model"
)

type Parser struct {
	warnFields    []string
	messageFields []string
	timeFields    []string
	warnKeywords  []string
	logIDKeys     []string
	logIDRegexps  []*regexp.Regexp
	unknownLogID  string
}

func New(cfg config.FilterConfig, unknownLogID string) (*Parser, error) {
	regexps := make([]*regexp.Regexp, 0, len(cfg.LogIDExtract.Regexps))
	for _, expr := range cfg.LogIDExtract.Regexps {
		re, err := regexp.Compile(expr)
		if err != nil {
			return nil, fmt.Errorf("compile log id regexp %q: %w", expr, err)
		}
		regexps = append(regexps, re)
	}

	p := &Parser{
		warnFields:    normalizeKeys(cfg.WarnMatch.JSONFields),
		messageFields: normalizeKeys(cfg.WarnMatch.MessageFields),
		timeFields:    normalizeKeys(cfg.WarnMatch.TimeFields),
		warnKeywords:  normalizeKeywords(cfg.WarnMatch.Keywords),
		logIDKeys:     normalizeKeys(cfg.LogIDExtract.JSONKeys),
		logIDRegexps:  regexps,
		unknownLogID:  unknownLogID,
	}

	if len(p.warnKeywords) == 0 {
		p.warnKeywords = []string{"WARN"}
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
	if json.Valid([]byte(line)) {
		if err := json.Unmarshal([]byte(line), &parsedJSON); err != nil {
			return nil, false, fmt.Errorf("unmarshal json log: %w", err)
		}
		event.ParsedFromJSON = true
		event.Message = coalesceString(parsedJSON, p.messageFields, line)
		if ts, ok := parseTimestamp(parsedJSON, p.timeFields); ok {
			event.Timestamp = ts
		}
		if level, ok := extractWarnLevel(parsedJSON, p.warnFields, p.warnKeywords); ok {
			event.Level = level
			event.AlertMatched = true
		}
		if logID, ok := extractLogIDFromJSON(parsedJSON, p.logIDKeys); ok {
			event.LogID = logID
		}
	}

	if event.Level == "" {
		level, ok := extractWarnLevelFromText(line, p.warnKeywords)
		if ok {
			event.Level = level
			event.AlertMatched = true
		}
	}

	if event.LogID == p.unknownLogID {
		if logID, ok := extractLogIDFromText(event.Message, p.logIDRegexps); ok {
			event.LogID = logID
		} else if logID, ok := extractLogIDFromText(line, p.logIDRegexps); ok {
			event.LogID = logID
		}
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

func normalizeKeywords(values []string) []string {
	out := make([]string, 0, len(values))
	for _, value := range values {
		if trimmed := strings.TrimSpace(strings.ToUpper(value)); trimmed != "" {
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
	layouts := []string{
		time.RFC3339Nano,
		time.RFC3339,
		"2006-01-02 15:04:05",
		"2006-01-02 15:04:05.000",
	}
	for _, layout := range layouts {
		ts, err := time.Parse(layout, value)
		if err == nil {
			return ts.UTC(), true
		}
	}
	if unixSeconds, err := strconv.ParseInt(value, 10, 64); err == nil {
		return time.Unix(unixSeconds, 0).UTC(), true
	}
	return time.Time{}, false
}

func extractWarnLevel(payload map[string]any, fields []string, keywords []string) (string, bool) {
	for _, wanted := range fields {
		for field, value := range payload {
			if !strings.EqualFold(field, wanted) {
				continue
			}
			level := strings.ToUpper(strings.TrimSpace(fmt.Sprint(value)))
			for _, keyword := range keywords {
				if level == keyword {
					return level, true
				}
			}
		}
	}
	return "", false
}

func extractWarnLevelFromText(line string, keywords []string) (string, bool) {
	upper := strings.ToUpper(line)
	for _, keyword := range keywords {
		if strings.Contains(upper, keyword) {
			return keyword, true
		}
	}
	return "", false
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
