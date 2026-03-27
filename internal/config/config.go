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
	Hosts           []DockerHostConfig `yaml:"hosts"`
	Host            string             `yaml:"host"`
	IncludePatterns []string           `yaml:"include_patterns"`
	Since           string             `yaml:"since"`
}

type DockerHostConfig struct {
	Host            string                     `yaml:"host"`
	IncludePatterns []string                   `yaml:"include_patterns"`
	Since           *string                    `yaml:"since"`
	Filters         *FilterOverrideConfig      `yaml:"filters"`
	Aggregation     *AggregationOverrideConfig `yaml:"aggregation"`
	DingTalk        *DingTalkOverrideConfig    `yaml:"dingtalk"`
	Storage         *StorageOverrideConfig     `yaml:"storage"`
}

type FilterOverrideConfig struct {
	WarnMatch    *WarnMatchOverrideConfig    `yaml:"warn_match"`
	LogIDExtract *LogIDExtractOverrideConfig `yaml:"log_id_extract"`
}

type WarnMatchOverrideConfig struct {
	Regexps        []string `yaml:"regexps"`
	ExcludeRegexps []string `yaml:"exclude_regexps"`
	JSONFields     []string `yaml:"json_fields"`
	MessageFields  []string `yaml:"message_fields"`
	TimeFields     []string `yaml:"time_fields"`
}

type LogIDExtractOverrideConfig struct {
	JSONKeys []string `yaml:"json_keys"`
	Regexps  []string `yaml:"regexps"`
}

type AggregationOverrideConfig struct {
	FlushSize     *int    `yaml:"flush_size"`
	FlushInterval *string `yaml:"flush_interval"`
	UnknownLogID  *string `yaml:"unknown_log_id"`
}

type DingTalkOverrideConfig struct {
	WebhookURL    *string  `yaml:"webhook_url"`
	Secret        *string  `yaml:"secret"`
	AtAll         *bool    `yaml:"at_all"`
	AtMobiles     []string `yaml:"at_mobiles"`
	MentionLevels []string `yaml:"mention_levels"`
	MaxEvents     *int     `yaml:"max_events"`
}

type StorageOverrideConfig struct {
	OutputDir *string `yaml:"output_dir"`
}

type ResolvedHostConfig struct {
	Host   string
	Config Config
}

func (c *DockerHostConfig) UnmarshalYAML(node *yaml.Node) error {
	switch node.Kind {
	case yaml.ScalarNode:
		return node.Decode(&c.Host)
	case yaml.MappingNode:
		type dockerHostAlias DockerHostConfig
		var decoded dockerHostAlias
		if err := node.Decode(&decoded); err != nil {
			return err
		}
		*c = DockerHostConfig(decoded)
		return nil
	default:
		return fmt.Errorf("docker host entry must be a string or map")
	}
}

func (c *DockerConfig) UnmarshalYAML(node *yaml.Node) error {
	for i := 0; i+1 < len(node.Content); i += 2 {
		keyNode := node.Content[i]
		valueNode := node.Content[i+1]
		switch keyNode.Value {
		case "host":
			hosts, err := decodeDockerHostEntries(valueNode)
			if err != nil {
				return fmt.Errorf("docker.host: %w", err)
			}
			if valueNode.Kind == yaml.ScalarNode {
				c.Host = hosts[0].Host
				continue
			}
			c.Hosts = append(c.Hosts, hosts...)
		case "hosts":
			hosts, err := decodeDockerHostEntries(valueNode)
			if err != nil {
				return fmt.Errorf("docker.hosts: %w", err)
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

func decodeDockerHostEntries(node *yaml.Node) ([]DockerHostConfig, error) {
	switch node.Kind {
	case yaml.ScalarNode, yaml.MappingNode:
		var host DockerHostConfig
		if err := node.Decode(&host); err != nil {
			return nil, err
		}
		return []DockerHostConfig{host}, nil
	case yaml.SequenceNode:
		hosts := make([]DockerHostConfig, 0, len(node.Content))
		for _, item := range node.Content {
			var host DockerHostConfig
			if err := item.Decode(&host); err != nil {
				return nil, err
			}
			hosts = append(hosts, host)
		}
		return hosts, nil
	default:
		return nil, fmt.Errorf("must be a string, map, or list")
	}
}

type FilterConfig struct {
	WarnMatch    WarnMatchConfig    `yaml:"warn_match"`
	LogIDExtract LogIDExtractConfig `yaml:"log_id_extract"`
}

type WarnMatchConfig struct {
	Regexps        []string `yaml:"regexps"`
	ExcludeRegexps []string `yaml:"exclude_regexps"`
	JSONFields     []string `yaml:"json_fields"`
	MessageFields  []string `yaml:"message_fields"`
	TimeFields     []string `yaml:"time_fields"`
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
	MaxEvents     int      `yaml:"max_events"`
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
				Regexps:       []string{`(?i)\b(WARN|ERROR)\b`},
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
			MaxEvents:     5,
		},
		Storage: StorageConfig{
			OutputDir: "data",
		},
	}
}

func (c Config) Validate() error {
	_, err := c.ResolveHosts()
	return err
}

func (c Config) validateResolved() error {
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
	if c.DingTalk.MaxEvents <= 0 {
		return errors.New("dingtalk.max_events must be greater than 0")
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
	entries := c.HostConfigsOrDefault()
	seen := make(map[string]struct{})
	hosts := make([]string, 0, len(entries))
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

	for _, entry := range entries {
		appendHost(entry.Host)
	}
	if len(hosts) == 0 {
		return []string{""}
	}
	return hosts
}

func (c DockerConfig) HostConfigsOrDefault() []DockerHostConfig {
	hosts := make([]DockerHostConfig, 0, len(c.Hosts)+1)
	if c.Host != "" {
		hosts = append(hosts, DockerHostConfig{Host: strings.TrimSpace(c.Host)})
	}
	for _, host := range c.Hosts {
		host.Host = strings.TrimSpace(host.Host)
		hosts = append(hosts, host)
	}
	if len(hosts) == 0 {
		return []DockerHostConfig{{Host: ""}}
	}
	return hosts
}

func (c Config) ResolveHosts() ([]ResolvedHostConfig, error) {
	hostConfigs := c.Docker.HostConfigsOrDefault()
	resolved := make([]ResolvedHostConfig, 0, len(hostConfigs))
	seen := make(map[string]struct{}, len(hostConfigs))

	for _, hostCfg := range hostConfigs {
		host := strings.TrimSpace(hostCfg.Host)
		if _, exists := seen[host]; exists {
			return nil, fmt.Errorf("duplicate docker host %q", hostDisplayName(host))
		}
		seen[host] = struct{}{}

		resolvedCfg := c
		resolvedCfg.Docker.Host = host
		resolvedCfg.Docker.Hosts = nil
		applyDockerHostOverrides(&resolvedCfg, hostCfg)

		if err := resolvedCfg.validateResolved(); err != nil {
			return nil, fmt.Errorf("docker host %q: %w", hostDisplayName(host), err)
		}

		resolved = append(resolved, ResolvedHostConfig{
			Host:   host,
			Config: resolvedCfg,
		})
	}

	return resolved, nil
}

func applyDockerHostOverrides(cfg *Config, hostCfg DockerHostConfig) {
	if hostCfg.IncludePatterns != nil {
		cfg.Docker.IncludePatterns = append([]string(nil), hostCfg.IncludePatterns...)
	}
	if hostCfg.Since != nil {
		cfg.Docker.Since = *hostCfg.Since
	}
	if hostCfg.Filters != nil {
		applyFilterOverrides(&cfg.Filters, hostCfg.Filters)
	}
	if hostCfg.Aggregation != nil {
		applyAggregationOverrides(&cfg.Aggregation, hostCfg.Aggregation)
	}
	if hostCfg.DingTalk != nil {
		applyDingTalkOverrides(&cfg.DingTalk, hostCfg.DingTalk)
	}
	if hostCfg.Storage != nil {
		applyStorageOverrides(&cfg.Storage, hostCfg.Storage)
	}
}

func applyFilterOverrides(dst *FilterConfig, override *FilterOverrideConfig) {
	if override.WarnMatch != nil {
		applyWarnMatchOverrides(&dst.WarnMatch, override.WarnMatch)
	}
	if override.LogIDExtract != nil {
		applyLogIDExtractOverrides(&dst.LogIDExtract, override.LogIDExtract)
	}
}

func applyWarnMatchOverrides(dst *WarnMatchConfig, override *WarnMatchOverrideConfig) {
	if override.Regexps != nil {
		dst.Regexps = append([]string(nil), override.Regexps...)
	}
	if override.ExcludeRegexps != nil {
		dst.ExcludeRegexps = append([]string(nil), override.ExcludeRegexps...)
	}
	if override.JSONFields != nil {
		dst.JSONFields = append([]string(nil), override.JSONFields...)
	}
	if override.MessageFields != nil {
		dst.MessageFields = append([]string(nil), override.MessageFields...)
	}
	if override.TimeFields != nil {
		dst.TimeFields = append([]string(nil), override.TimeFields...)
	}
}

func applyLogIDExtractOverrides(dst *LogIDExtractConfig, override *LogIDExtractOverrideConfig) {
	if override.JSONKeys != nil {
		dst.JSONKeys = append([]string(nil), override.JSONKeys...)
	}
	if override.Regexps != nil {
		dst.Regexps = append([]string(nil), override.Regexps...)
	}
}

func applyAggregationOverrides(dst *AggregationConfig, override *AggregationOverrideConfig) {
	if override.FlushSize != nil {
		dst.FlushSize = *override.FlushSize
	}
	if override.FlushInterval != nil {
		dst.FlushInterval = *override.FlushInterval
	}
	if override.UnknownLogID != nil {
		dst.UnknownLogID = *override.UnknownLogID
	}
}

func applyDingTalkOverrides(dst *DingTalkConfig, override *DingTalkOverrideConfig) {
	if override.WebhookURL != nil {
		dst.WebhookURL = *override.WebhookURL
	}
	if override.Secret != nil {
		dst.Secret = *override.Secret
	}
	if override.AtAll != nil {
		dst.AtAll = *override.AtAll
	}
	if override.AtMobiles != nil {
		dst.AtMobiles = append([]string(nil), override.AtMobiles...)
	}
	if override.MentionLevels != nil {
		dst.MentionLevels = append([]string(nil), override.MentionLevels...)
	}
	if override.MaxEvents != nil {
		dst.MaxEvents = *override.MaxEvents
	}
}

func applyStorageOverrides(dst *StorageConfig, override *StorageOverrideConfig) {
	if override.OutputDir != nil {
		dst.OutputDir = *override.OutputDir
	}
}

func hostDisplayName(host string) string {
	if strings.TrimSpace(host) == "" {
		return "local"
	}
	return host
}
