package parser

import (
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
