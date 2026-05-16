package parser

import (
	"fmt"
	"testing"
	"time"

	"github.com/geekeryy/docker-monitor/internal/config"
	"github.com/geekeryy/docker-monitor/internal/model"
)

func TestParseJSONWarnLog(t *testing.T) {
	t.Parallel()

	p, err := New(config.FilterConfig{
		WarnMatch: config.WarnMatchConfig{
			Regexps:       []string{`(?i)\b(WARN)\b`},
			JSONFields:    []string{"level"},
			MessageFields: []string{"message"},
			TimeFields:    []string{"time"},
		},
		LogIDExtract: config.LogIDExtractConfig{
			JSONKeys: []string{"log_id"},
		},
	}, "unknown")
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	raw := model.RawLog{
		Timestamp: time.Unix(10, 0).UTC(),
		Container: model.ContainerInfo{Name: "app-1"},
		Stream:    "stdout",
		Line:      `{"time":"2026-03-24T10:20:30Z","level":"WARN","log_id":"warn-1","message":"disk almost full"}`,
	}

	event, ok, err := p.Parse(raw)
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	if !ok {
		t.Fatalf("Parse() ok = false, want true")
	}
	if event.LogID != "warn-1" {
		t.Fatalf("event.LogID = %q, want %q", event.LogID, "warn-1")
	}
	if !event.AlertMatched {
		t.Fatalf("event.AlertMatched = false, want true")
	}
	if event.Message != "disk almost full" {
		t.Fatalf("event.Message = %q, want %q", event.Message, "disk almost full")
	}
	if !event.Timestamp.Equal(time.Date(2026, 3, 24, 10, 20, 30, 0, time.UTC)) {
		t.Fatalf("event.Timestamp = %v, want parsed JSON timestamp", event.Timestamp)
	}
}

func TestParseTextWarnLogWithRegexFallback(t *testing.T) {
	t.Parallel()

	p, err := New(config.FilterConfig{
		WarnMatch: config.WarnMatchConfig{
			Regexps: []string{`(?i)\b(WARN)\b`},
		},
		LogIDExtract: config.LogIDExtractConfig{
			Regexps: []string{`(?i)\blog[_-]?id=([A-Za-z0-9._:-]+)`},
		},
	}, "unknown")
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	event, ok, err := p.Parse(model.RawLog{
		Timestamp: time.Unix(20, 0).UTC(),
		Container: model.ContainerInfo{Name: "worker-1"},
		Stream:    "stderr",
		Line:      "2026-03-24 10:20:30 WARN queue lag detected log_id=job-42",
	})
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	if !ok {
		t.Fatalf("Parse() ok = false, want true")
	}
	if event.LogID != "job-42" {
		t.Fatalf("event.LogID = %q, want %q", event.LogID, "job-42")
	}
	if event.Level != "WARN" {
		t.Fatalf("event.Level = %q, want %q", event.Level, "WARN")
	}
	if !event.AlertMatched {
		t.Fatalf("event.AlertMatched = false, want true")
	}
}

func TestParseNonWarnLogWithID(t *testing.T) {
	t.Parallel()

	p, err := New(config.FilterConfig{
		WarnMatch: config.WarnMatchConfig{
			Regexps:       []string{`(?i)\b(WARN)\b`},
			JSONFields:    []string{"level"},
			MessageFields: []string{"message"},
			TimeFields:    []string{"time"},
		},
		LogIDExtract: config.LogIDExtractConfig{
			JSONKeys: []string{"id"},
		},
	}, "unknown")
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	event, ok, err := p.Parse(model.RawLog{
		Timestamp: time.Unix(40, 0).UTC(),
		Container: model.ContainerInfo{Name: "app-1"},
		Stream:    "stdout",
		Line:      `{"time":"2026-03-24T10:20:30Z","level":"INFO","id":"same-1","message":"request started"}`,
	})
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	if !ok {
		t.Fatalf("Parse() ok = false, want true")
	}
	if event.LogID != "same-1" {
		t.Fatalf("event.LogID = %q, want %q", event.LogID, "same-1")
	}
	if event.AlertMatched {
		t.Fatalf("event.AlertMatched = true, want false")
	}
}

func TestParseSkipsNonWarnLogs(t *testing.T) {
	t.Parallel()

	p, err := New(config.FilterConfig{
		WarnMatch: config.WarnMatchConfig{
			Regexps: []string{`(?i)\b(WARN)\b`},
		},
	}, "unknown")
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	_, ok, err := p.Parse(model.RawLog{
		Timestamp: time.Unix(30, 0).UTC(),
		Line:      "INFO service healthy",
	})
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	if ok {
		t.Fatalf("Parse() ok = true, want false")
	}
}

func TestParseSkipsTextLogMatchedByExcludeKeyword(t *testing.T) {
	t.Parallel()

	p, err := New(config.FilterConfig{
		WarnMatch: config.WarnMatchConfig{
			Regexps:        []string{`(?i)\b(WARN)\b`},
			ExcludeRegexps: []string{`(?i)IGNORE_ME`},
		},
		LogIDExtract: config.LogIDExtractConfig{
			Regexps: []string{`(?i)\blog[_-]?id=([A-Za-z0-9._:-]+)`},
		},
	}, "unknown")
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	_, ok, err := p.Parse(model.RawLog{
		Timestamp: time.Unix(50, 0).UTC(),
		Container: model.ContainerInfo{Name: "worker-1"},
		Stream:    "stderr",
		Line:      "2026-03-24 10:20:30 WARN queue lag detected IGNORE_ME log_id=job-42",
	})
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	if ok {
		t.Fatalf("Parse() ok = true, want false")
	}
}

func TestParseSkipsJSONLogMatchedByExcludeKeyword(t *testing.T) {
	t.Parallel()

	p, err := New(config.FilterConfig{
		WarnMatch: config.WarnMatchConfig{
			Regexps:        []string{`(?i)\b(ERROR)\b`},
			ExcludeRegexps: []string{`(?i)IGNORE_ME`},
			JSONFields:     []string{"level"},
			MessageFields:  []string{"message"},
		},
		LogIDExtract: config.LogIDExtractConfig{
			JSONKeys: []string{"log_id"},
		},
	}, "unknown")
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	_, ok, err := p.Parse(model.RawLog{
		Timestamp: time.Unix(60, 0).UTC(),
		Container: model.ContainerInfo{Name: "app-1"},
		Stream:    "stdout",
		Line:      `{"level":"ERROR","log_id":"err-1","message":"IGNORE_ME create order failed"}`,
	})
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	if ok {
		t.Fatalf("Parse() ok = true, want false")
	}
}

func TestParseSkipsTextLogMatchedByExcludeRegexp(t *testing.T) {
	t.Parallel()

	p, err := New(config.FilterConfig{
		WarnMatch: config.WarnMatchConfig{
			Regexps:        []string{`(?i)\b(WARN)\b`},
			ExcludeRegexps: []string{`timeout=\d+ms`},
		},
		LogIDExtract: config.LogIDExtractConfig{
			Regexps: []string{`(?i)\blog[_-]?id=([A-Za-z0-9._:-]+)`},
		},
	}, "unknown")
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	_, ok, err := p.Parse(model.RawLog{
		Timestamp: time.Unix(70, 0).UTC(),
		Container: model.ContainerInfo{Name: "worker-1"},
		Stream:    "stderr",
		Line:      "2026-03-24 10:20:30 WARN retry timeout=1234ms log_id=job-42",
	})
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	if ok {
		t.Fatalf("Parse() ok = true, want false")
	}
}

func TestParseSkipsJSONLogMatchedByExcludeRegexp(t *testing.T) {
	t.Parallel()

	p, err := New(config.FilterConfig{
		WarnMatch: config.WarnMatchConfig{
			Regexps:        []string{`(?i)\b(ERROR)\b`},
			ExcludeRegexps: []string{`(?i)deadline.*exceeded`},
			JSONFields:     []string{"level"},
			MessageFields:  []string{"message"},
		},
		LogIDExtract: config.LogIDExtractConfig{
			JSONKeys: []string{"log_id"},
		},
	}, "unknown")
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	_, ok, err := p.Parse(model.RawLog{
		Timestamp: time.Unix(80, 0).UTC(),
		Container: model.ContainerInfo{Name: "app-1"},
		Stream:    "stdout",
		Line:      `{"level":"ERROR","log_id":"err-1","message":"request deadline was exceeded"}`,
	})
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	if ok {
		t.Fatalf("Parse() ok = true, want false")
	}
}

func TestParseSkipsJSONLogMatchedByExcludeRegexpInNestedField(t *testing.T) {
	t.Parallel()

	p, err := New(config.FilterConfig{
		WarnMatch: config.WarnMatchConfig{
			Regexps:        []string{`(?i)\b(ERROR)\b`},
			ExcludeRegexps: []string{`(?i)\bUnauthorized\b`},
			JSONFields:     []string{"level"},
		},
		LogIDExtract: config.LogIDExtractConfig{
			JSONKeys: []string{"log_id"},
		},
	}, "unknown")
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	_, ok, err := p.Parse(model.RawLog{
		Timestamp: time.Unix(81, 0).UTC(),
		Container: model.ContainerInfo{Name: "app-1"},
		Stream:    "stdout",
		Line:      `{"level":"ERROR","log_id":"err-1","error":{"message":"\u0055nauthorized"}}`,
	})
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	if ok {
		t.Fatalf("Parse() ok = true, want false")
	}
}

func TestParseTimestampStringPreservesExplicitOffset(t *testing.T) {
	t.Parallel()

	ts, ok := parseTimestampString("2026-03-24T10:20:30+08:00")
	if !ok {
		t.Fatal("parseTimestampString() ok = false, want true")
	}
	if got := ts.Format(time.RFC3339); got != "2026-03-24T10:20:30+08:00" {
		t.Fatalf("timestamp = %q, want preserved offset", got)
	}
}

func TestParseTimestampStringUsesLocalZoneForNaiveTimestamp(t *testing.T) {
	t.Parallel()

	ts, ok := parseTimestampString("2026-03-24 10:20:30")
	if !ok {
		t.Fatal("parseTimestampString() ok = false, want true")
	}
	expected := time.Date(2026, 3, 24, 10, 20, 30, 0, time.Local)
	if !ts.Equal(expected) {
		t.Fatalf("timestamp = %s, want %s", ts, expected)
	}
	if ts.Location() != time.Local {
		t.Fatalf("location = %v, want time.Local", ts.Location())
	}
}

func TestNewRejectsInvalidExcludeRegexp(t *testing.T) {
	t.Parallel()

	_, err := New(config.FilterConfig{
		WarnMatch: config.WarnMatchConfig{
			ExcludeRegexps: []string{"("},
		},
	}, "unknown")
	if err == nil {
		t.Fatal("New() error = nil, want error")
	}
}

func TestParseSkipsLogContainingExcludedIP(t *testing.T) {
	t.Parallel()

	p, err := New(config.FilterConfig{
		WarnMatch: config.WarnMatchConfig{
			Regexps: []string{`(?i)\b(WARN|ERROR)\b`},
			ExcludeIPs: []string{
				"106.55.202.118",
				"113.96.223.69",
			},
			JSONFields:    []string{"level"},
			MessageFields: []string{"message"},
		},
		LogIDExtract: config.LogIDExtractConfig{
			JSONKeys: []string{"log_id"},
		},
	}, "unknown")
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	cases := []struct {
		name string
		line string
	}{
		{
			name: "text log carrying whitelisted ip",
			line: "2026-03-24 10:20:30 WARN suspicious request from 106.55.202.118 log_id=job-1",
		},
		{
			name: "json log carrying whitelisted ip in nested field",
			line: `{"level":"ERROR","log_id":"err-1","client":{"ip":"113.96.223.69"},"message":"login failed"}`,
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			_, ok, err := p.Parse(model.RawLog{
				Timestamp: time.Unix(90, 0).UTC(),
				Container: model.ContainerInfo{Name: "app-1"},
				Stream:    "stderr",
				Line:      tc.line,
			})
			if err != nil {
				t.Fatalf("Parse() error = %v", err)
			}
			if ok {
				t.Fatalf("Parse() ok = true, want false (line should be filtered)")
			}
		})
	}
}

func TestParseKeepsLogWithSimilarButDifferentIP(t *testing.T) {
	t.Parallel()

	p, err := New(config.FilterConfig{
		WarnMatch: config.WarnMatchConfig{
			Regexps:    []string{`(?i)\b(WARN|ERROR)\b`},
			ExcludeIPs: []string{"1.1.1.1"},
		},
		LogIDExtract: config.LogIDExtractConfig{
			Regexps: []string{`(?i)\blog[_-]?id=([A-Za-z0-9._:-]+)`},
		},
	}, "unknown")
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	event, ok, err := p.Parse(model.RawLog{
		Timestamp: time.Unix(100, 0).UTC(),
		Container: model.ContainerInfo{Name: "app-1"},
		Stream:    "stderr",
		Line:      "2026-03-24 10:20:30 WARN request from 1.1.1.10 log_id=job-7",
	})
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	if !ok {
		t.Fatal("Parse() ok = false, want true (1.1.1.10 must not match 1.1.1.1)")
	}
	if event.LogID != "job-7" {
		t.Fatalf("event.LogID = %q, want %q", event.LogID, "job-7")
	}
}

func TestParseIgnoresBlankExcludeIPs(t *testing.T) {
	t.Parallel()

	p, err := New(config.FilterConfig{
		WarnMatch: config.WarnMatchConfig{
			Regexps:    []string{`(?i)\b(WARN)\b`},
			ExcludeIPs: []string{"", "  "},
		},
		LogIDExtract: config.LogIDExtractConfig{
			Regexps: []string{`(?i)\blog[_-]?id=([A-Za-z0-9._:-]+)`},
		},
	}, "unknown")
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	event, ok, err := p.Parse(model.RawLog{
		Timestamp: time.Unix(110, 0).UTC(),
		Container: model.ContainerInfo{Name: "app-1"},
		Stream:    "stderr",
		Line:      "2026-03-24 10:20:30 WARN normal request log_id=job-8",
	})
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	if !ok {
		t.Fatal("Parse() ok = false, want true")
	}
	if event.LogID != "job-8" {
		t.Fatalf("event.LogID = %q, want %q", event.LogID, "job-8")
	}
}

func TestParseChainExcludeSilencesFollowingLogsByLogID(t *testing.T) {
	t.Parallel()

	p, err := New(config.FilterConfig{
		WarnMatch: config.WarnMatchConfig{
			Regexps:         []string{`(?i)\b(WARN|ERROR)\b`},
			ExcludeIPs:      []string{"106.55.202.118"},
			ExcludeChainTTL: "10m",
		},
		LogIDExtract: config.LogIDExtractConfig{
			JSONKeys: []string{"trace_id"},
			Regexps:  []string{`(?i)\btrace[_-]?id=([A-Za-z0-9._:-]+)`},
		},
	}, "unknown")
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	base := time.Date(2026, 5, 14, 10, 0, 0, 0, time.UTC)
	p.SetNowFunc(func() time.Time { return base })

	// 第 1 条：带白名单 IP + trace_id，应被过滤并登记链路
	_, ok, err := p.Parse(model.RawLog{
		Timestamp: base,
		Container: model.ContainerInfo{Name: "app-1"},
		Stream:    "stderr",
		Line:      "WARN request from 106.55.202.118 trace_id=req-1",
	})
	if err != nil || ok {
		t.Fatalf("first line should be excluded; ok=%v err=%v", ok, err)
	}

	// 第 2 条：JSON 格式，同 trace_id 但不带 IP，应被链路级静默
	_, ok, err = p.Parse(model.RawLog{
		Timestamp: base.Add(2 * time.Second),
		Container: model.ContainerInfo{Name: "app-1"},
		Stream:    "stdout",
		Line:      `{"level":"ERROR","trace_id":"req-1","message":"auth failed"}`,
	})
	if err != nil || ok {
		t.Fatalf("follow-up log with same trace_id should be silenced; ok=%v err=%v", ok, err)
	}

	// 第 3 条：纯文本，同 trace_id 但不带 IP，应被链路级静默
	_, ok, err = p.Parse(model.RawLog{
		Timestamp: base.Add(3 * time.Second),
		Container: model.ContainerInfo{Name: "app-1"},
		Stream:    "stderr",
		Line:      "ERROR 401 unauthorized trace_id=req-1",
	})
	if err != nil || ok {
		t.Fatalf("text follow-up with same trace_id should be silenced; ok=%v err=%v", ok, err)
	}

	// 第 4 条：不同 trace_id，应正常告警
	event, ok, err := p.Parse(model.RawLog{
		Timestamp: base.Add(4 * time.Second),
		Container: model.ContainerInfo{Name: "app-1"},
		Stream:    "stderr",
		Line:      "ERROR something broken trace_id=req-2",
	})
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	if !ok {
		t.Fatal("unrelated trace_id should not be silenced")
	}
	if event.LogID != "req-2" {
		t.Fatalf("event.LogID = %q, want %q", event.LogID, "req-2")
	}
}

func TestParseChainExcludeExpiresAfterTTL(t *testing.T) {
	t.Parallel()

	p, err := New(config.FilterConfig{
		WarnMatch: config.WarnMatchConfig{
			Regexps:         []string{`(?i)\b(WARN|ERROR)\b`},
			ExcludeIPs:      []string{"106.55.202.118"},
			ExcludeChainTTL: "1m",
		},
		LogIDExtract: config.LogIDExtractConfig{
			Regexps: []string{`(?i)\btrace[_-]?id=([A-Za-z0-9._:-]+)`},
		},
	}, "unknown")
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	current := time.Date(2026, 5, 14, 10, 0, 0, 0, time.UTC)
	p.SetNowFunc(func() time.Time { return current })

	_, ok, _ := p.Parse(model.RawLog{
		Timestamp: current,
		Container: model.ContainerInfo{Name: "app-1"},
		Stream:    "stderr",
		Line:      "WARN scan from 106.55.202.118 trace_id=req-9",
	})
	if ok {
		t.Fatal("first line should be excluded")
	}

	// TTL 内：链路静默
	current = current.Add(30 * time.Second)
	_, ok, _ = p.Parse(model.RawLog{
		Timestamp: current,
		Container: model.ContainerInfo{Name: "app-1"},
		Stream:    "stderr",
		Line:      "ERROR follow-up trace_id=req-9",
	})
	if ok {
		t.Fatal("within TTL the follow-up should still be silenced")
	}

	// TTL 过期：恢复告警
	current = current.Add(2 * time.Minute)
	event, ok, err := p.Parse(model.RawLog{
		Timestamp: current,
		Container: model.ContainerInfo{Name: "app-1"},
		Stream:    "stderr",
		Line:      "ERROR late tail trace_id=req-9",
	})
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	if !ok {
		t.Fatal("after TTL the log with same trace_id must alert again")
	}
	if event.LogID != "req-9" {
		t.Fatalf("event.LogID = %q, want %q", event.LogID, "req-9")
	}
}

func TestParseChainExcludeDisabledWhenTTLZero(t *testing.T) {
	t.Parallel()

	p, err := New(config.FilterConfig{
		WarnMatch: config.WarnMatchConfig{
			Regexps:         []string{`(?i)\b(WARN|ERROR)\b`},
			ExcludeIPs:      []string{"106.55.202.118"},
			ExcludeChainTTL: "",
		},
		LogIDExtract: config.LogIDExtractConfig{
			Regexps: []string{`(?i)\btrace[_-]?id=([A-Za-z0-9._:-]+)`},
		},
	}, "unknown")
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	if p.chainBlocker != nil {
		t.Fatal("chainBlocker should be nil when TTL is empty")
	}

	_, ok, _ := p.Parse(model.RawLog{
		Timestamp: time.Unix(200, 0).UTC(),
		Line:      "WARN scan from 106.55.202.118 trace_id=req-1",
	})
	if ok {
		t.Fatal("first line should still be filtered by IP whitelist")
	}

	event, ok, err := p.Parse(model.RawLog{
		Timestamp: time.Unix(201, 0).UTC(),
		Line:      "ERROR follow-up trace_id=req-1",
	})
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	if !ok {
		t.Fatal("with chain filtering disabled, follow-up must alert")
	}
	if event.LogID != "req-1" {
		t.Fatalf("event.LogID = %q, want %q", event.LogID, "req-1")
	}
}

func TestParseChainExcludeIgnoresUnknownLogID(t *testing.T) {
	t.Parallel()

	p, err := New(config.FilterConfig{
		WarnMatch: config.WarnMatchConfig{
			Regexps:         []string{`(?i)\b(WARN|ERROR)\b`},
			ExcludeIPs:      []string{"106.55.202.118"},
			ExcludeChainTTL: "10m",
		},
		LogIDExtract: config.LogIDExtractConfig{
			Regexps: []string{`(?i)\btrace[_-]?id=([A-Za-z0-9._:-]+)`},
		},
	}, "unknown")
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	base := time.Date(2026, 5, 14, 10, 0, 0, 0, time.UTC)
	p.SetNowFunc(func() time.Time { return base })

	// 命中 IP 但无 log_id：仅过滤本条，绝对不能登记 unknownLogID
	_, ok, _ := p.Parse(model.RawLog{
		Timestamp: base,
		Line:      "WARN scan from 106.55.202.118 with no trace info",
	})
	if ok {
		t.Fatal("line containing whitelist IP should be filtered")
	}

	// 后续无 log_id 的 ERROR 不应被错误静默——
	// 它会按已有的 "无 log_id 的告警" 走单条立即输出流程
	event, ok, err := p.Parse(model.RawLog{
		Timestamp: base.Add(time.Second),
		Line:      "ERROR unrelated failure",
	})
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	if !ok {
		t.Fatal("unrelated ERROR without log_id must not be silenced by previous excluded log")
	}
	if event.LogID != "unknown" {
		t.Fatalf("event.LogID = %q, want %q", event.LogID, "unknown")
	}
}

func TestParseChainExcludeWorksWithJSONNestedIP(t *testing.T) {
	t.Parallel()

	p, err := New(config.FilterConfig{
		WarnMatch: config.WarnMatchConfig{
			Regexps:         []string{`(?i)\b(WARN|ERROR)\b`},
			ExcludeIPs:      []string{"113.96.223.69"},
			ExcludeChainTTL: "10m",
			JSONFields:      []string{"level"},
		},
		LogIDExtract: config.LogIDExtractConfig{
			JSONKeys: []string{"trace_id"},
		},
	}, "unknown")
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	base := time.Date(2026, 5, 14, 10, 0, 0, 0, time.UTC)
	p.SetNowFunc(func() time.Time { return base })

	_, ok, _ := p.Parse(model.RawLog{
		Timestamp: base,
		Line:      `{"level":"ERROR","trace_id":"chain-1","client":{"ip":"113.96.223.69"},"message":"login"}`,
	})
	if ok {
		t.Fatal("first JSON log with whitelisted IP should be excluded")
	}

	_, ok, _ = p.Parse(model.RawLog{
		Timestamp: base.Add(time.Second),
		Line:      `{"level":"ERROR","trace_id":"chain-1","message":"audit follow-up"}`,
	})
	if ok {
		t.Fatal("follow-up JSON log with same trace_id should be silenced")
	}
}

func TestNewRejectsInvalidExcludeChainTTL(t *testing.T) {
	t.Parallel()

	_, err := New(config.FilterConfig{
		WarnMatch: config.WarnMatchConfig{
			ExcludeChainTTL: "abc",
		},
	}, "unknown")
	if err == nil {
		t.Fatal("New() error = nil, want error")
	}
}

func TestChainBlockerNilSafeWhenTTLZero(t *testing.T) {
	t.Parallel()

	var b *chainBlocker
	b = newChainBlocker(0, 100)
	if b != nil {
		t.Fatal("newChainBlocker(0, _) should return nil")
	}
	// nil 安全调用
	b.Block("any", time.Now())
	if b.Blocked("any", time.Now()) {
		t.Fatal("nil blocker should never report blocked")
	}
}

func TestChainBlockerEvictsWhenOverCapacity(t *testing.T) {
	t.Parallel()

	b := newChainBlocker(10*time.Minute, 4)
	now := time.Date(2026, 5, 14, 10, 0, 0, 0, time.UTC)
	for i := 0; i < 6; i++ {
		b.Block(fmt.Sprintf("id-%d", i), now.Add(time.Duration(i)*time.Second))
	}

	// 容量 4，写入 6 条后触发淘汰，应稳定在 4 以内
	b.mu.Lock()
	size := len(b.entries)
	b.mu.Unlock()
	if size > 4 {
		t.Fatalf("entries size = %d, want <= 4 after eviction", size)
	}
}

func TestNewRejectsInvalidWarnRegexp(t *testing.T) {
	t.Parallel()

	_, err := New(config.FilterConfig{
		WarnMatch: config.WarnMatchConfig{
			Regexps: []string{"("},
		},
	}, "unknown")
	if err == nil {
		t.Fatal("New() error = nil, want error")
	}
}
