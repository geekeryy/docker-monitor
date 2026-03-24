package config

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

type Config struct {
	Docker      DockerConfig      `yaml:"docker"`
	Filters     FilterConfig      `yaml:"filters"`
	Aggregation AggregationConfig `yaml:"aggregation"`
	DingTalk    DingTalkConfig    `yaml:"dingtalk"`
	Storage     StorageConfig     `yaml:"storage"`
}

type DockerConfig struct {
	Hosts           []string `yaml:"hosts"`
	Host            string   `yaml:"host"`
	IncludePatterns []string `yaml:"include_patterns"`
	Since           string   `yaml:"since"`
}

func (c *DockerConfig) UnmarshalYAML(node *yaml.Node) error {
	type dockerConfigRaw struct {
		Hosts           []string `yaml:"hosts"`
		Host            string   `yaml:"host"`
		IncludePatterns []string `yaml:"include_patterns"`
		Since           string   `yaml:"since"`
	}

	var raw dockerConfigRaw
	if err := node.Decode(&raw); err == nil {
		*c = DockerConfig(raw)
		return nil
	}

	c.Host = ""
	c.Hosts = nil
	c.IncludePatterns = nil
	c.Since = ""

	for i := 0; i+1 < len(node.Content); i += 2 {
		keyNode := node.Content[i]
		valueNode := node.Content[i+1]
		switch keyNode.Value {
		case "host":
			switch valueNode.Kind {
			case yaml.ScalarNode:
				if err := valueNode.Decode(&c.Host); err != nil {
					return err
				}
			case yaml.SequenceNode:
				if err := valueNode.Decode(&c.Hosts); err != nil {
					return err
				}
			default:
				return fmt.Errorf("docker.host must be a string or list")
			}
		case "hosts":
			var hosts []string
			if err := valueNode.Decode(&hosts); err != nil {
				return err
			}
			c.Hosts = append(c.Hosts, hosts...)
		case "include_patterns":
			if err := valueNode.Decode(&c.IncludePatterns); err != nil {
				return err
			}
		case "since":
			if err := valueNode.Decode(&c.Since); err != nil {
				return err
			}
		}
	}

	return nil
}

type FilterConfig struct {
	WarnMatch    WarnMatchConfig    `yaml:"warn_match"`
	LogIDExtract LogIDExtractConfig `yaml:"log_id_extract"`
}

type WarnMatchConfig struct {
	Keywords      []string `yaml:"keywords"`
	JSONFields    []string `yaml:"json_fields"`
	MessageFields []string `yaml:"message_fields"`
	TimeFields    []string `yaml:"time_fields"`
}

type LogIDExtractConfig struct {
	JSONKeys []string `yaml:"json_keys"`
	Regexps  []string `yaml:"regexps"`
}

type AggregationConfig struct {
	FlushSize     int    `yaml:"flush_size"`
	FlushInterval string `yaml:"flush_interval"`
	UnknownLogID  string `yaml:"unknown_log_id"`
}

type DingTalkConfig struct {
	WebhookURL    string   `yaml:"webhook_url"`
	Secret        string   `yaml:"secret"`
	AtAll         bool     `yaml:"at_all"`
	AtMobiles     []string `yaml:"at_mobiles"`
	MentionLevels []string `yaml:"mention_levels"`
}

type StorageConfig struct {
	OutputDir string `yaml:"output_dir"`
}

func Load(path string) (Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Config{}, fmt.Errorf("read config: %w", err)
	}

	cfg := defaultConfig()
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return Config{}, fmt.Errorf("unmarshal config: %w", err)
	}

	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}

	return cfg, nil
}

func defaultConfig() Config {
	return Config{
		Docker: DockerConfig{
			IncludePatterns: []string{"*"},
			Since:           "0s",
		},
		Filters: FilterConfig{
			WarnMatch: WarnMatchConfig{
				Keywords:      []string{"WARN", "ERROR"},
				JSONFields:    []string{"level", "severity"},
				MessageFields: []string{"message", "msg", "log"},
				TimeFields:    []string{"time", "timestamp", "@timestamp", "ts"},
			},
			LogIDExtract: LogIDExtractConfig{
				JSONKeys: []string{"log_id", "logId", "alert_id", "alertId", "trace_id", "traceId"},
				Regexps: []string{
					`(?i)\blog[_-]?id["'=:\s]+([A-Za-z0-9._:-]+)`,
					`(?i)\balert[_-]?id["'=:\s]+([A-Za-z0-9._:-]+)`,
					`(?i)\btrace[_-]?id["'=:\s]+([A-Za-z0-9._:-]+)`,
				},
			},
		},
		Aggregation: AggregationConfig{
			FlushSize:     20,
			FlushInterval: "10s",
			UnknownLogID:  "unknown",
		},
		DingTalk: DingTalkConfig{
			AtMobiles:     []string{},
			MentionLevels: []string{"ERROR"},
		},
		Storage: StorageConfig{
			OutputDir: "data",
		},
	}
}

func (c Config) Validate() error {
	if len(c.Docker.IncludePatterns) == 0 {
		return errors.New("docker.include_patterns must not be empty")
	}
	if c.Storage.OutputDir == "" {
		return errors.New("storage.output_dir must not be empty")
	}
	if c.Aggregation.FlushSize <= 0 {
		return errors.New("aggregation.flush_size must be greater than 0")
	}
	if _, err := c.SinceDuration(); err != nil {
		return err
	}
	if _, err := c.FlushIntervalDuration(); err != nil {
		return err
	}
	if c.Aggregation.UnknownLogID == "" {
		return errors.New("aggregation.unknown_log_id must not be empty")
	}
	if c.DingTalk.Secret != "" && c.DingTalk.WebhookURL == "" {
		return errors.New("dingtalk.webhook_url must not be empty when dingtalk.secret is set")
	}
	return nil
}

func (c Config) SinceDuration() (time.Duration, error) {
	d, err := time.ParseDuration(c.Docker.Since)
	if err != nil {
		return 0, fmt.Errorf("parse docker.since: %w", err)
	}
	return d, nil
}

func (c Config) FlushIntervalDuration() (time.Duration, error) {
	d, err := time.ParseDuration(c.Aggregation.FlushInterval)
	if err != nil {
		return 0, fmt.Errorf("parse aggregation.flush_interval: %w", err)
	}
	return d, nil
}

func (c DockerConfig) HostsOrDefault() []string {
	seen := make(map[string]struct{})
	hosts := make([]string, 0, len(c.Hosts)+1)
	appendHost := func(host string) {
		host = strings.TrimSpace(host)
		if host == "" {
			return
		}
		if _, ok := seen[host]; ok {
			return
		}
		seen[host] = struct{}{}
		hosts = append(hosts, host)
	}

	appendHost(c.Host)
	for _, host := range c.Hosts {
		appendHost(host)
	}
	if len(hosts) == 0 {
		return []string{""}
	}
	return hosts
}
