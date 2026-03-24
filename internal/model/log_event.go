package model

import "time"

type ContainerInfo struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type RawLog struct {
	Timestamp time.Time     `json:"timestamp"`
	Container ContainerInfo `json:"container"`
	Stream    string        `json:"stream"`
	Line      string        `json:"line"`
}

type LogEvent struct {
	Timestamp      time.Time     `json:"timestamp"`
	Container      ContainerInfo `json:"container"`
	Stream         string        `json:"stream"`
	Level          string        `json:"level"`
	LogID          string        `json:"log_id"`
	AlertMatched   bool          `json:"alert_matched"`
	Message        string        `json:"message"`
	Raw            string        `json:"raw"`
	ParsedFromJSON bool          `json:"parsed_from_json"`
}

type LogBatch struct {
	LogID      string     `json:"log_id"`
	FirstSeen  time.Time  `json:"first_seen"`
	LastSeen   time.Time  `json:"last_seen"`
	Count      int        `json:"count"`
	Containers []string   `json:"containers"`
	Events     []LogEvent `json:"events"`
	FlushedAt  time.Time  `json:"flushed_at"`
}
