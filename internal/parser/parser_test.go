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
			Keywords:      []string{"WARN"},
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
			Keywords: []string{"WARN"},
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
			Keywords:      []string{"WARN"},
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
			Keywords: []string{"WARN"},
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
