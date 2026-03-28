package config

import (
	"os"
	"reflect"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestDockerHostsOrDefault(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		cfg  DockerConfig
		want []string
	}{
		{
			name: "falls back to implicit local host",
			cfg:  DockerConfig{},
			want: []string{""},
		},
		{
			name: "uses single host",
			cfg: DockerConfig{
				Host: "unix:///var/run/docker.sock",
			},
			want: []string{"unix:///var/run/docker.sock"},
		},
		{
			name: "merges legacy host and hosts list",
			cfg: DockerConfig{
				Host: "unix:///var/run/docker.sock",
				Hosts: []DockerHostConfig{
					{Host: "ssh://coach_test"},
					{Host: "unix:///var/run/docker.sock"},
					{Host: " tcp://127.0.0.1:2375 "},
				},
			},
			want: []string{"unix:///var/run/docker.sock", "ssh://coach_test", "tcp://127.0.0.1:2375"},
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := tt.cfg.HostsOrDefault(); !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("HostsOrDefault() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestDockerConfigUnmarshalYAML(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		data string
		want DockerConfig
	}{
		{
			name: "supports legacy single host string",
			data: "host: unix:///var/run/docker.sock\ninclude_patterns: [\"app-*\"]\nsince: 0s\n",
			want: DockerConfig{
				Host:            "unix:///var/run/docker.sock",
				IncludePatterns: []string{"app-*"},
				Since:           "0s",
			},
		},
		{
			name: "supports list under host key",
			data: "host:\n  - unix:///var/run/docker.sock\n  - ssh://coach_test\ninclude_patterns: [\"app-*\"]\nsince: 0s\n",
			want: DockerConfig{
				Hosts: []DockerHostConfig{
					{Host: "unix:///var/run/docker.sock"},
					{Host: "ssh://coach_test"},
				},
				IncludePatterns: []string{"app-*"},
				Since:           "0s",
			},
		},
		{
			name: "merges host and hosts",
			data: "host: unix:///var/run/docker.sock\nhosts:\n  - ssh://coach_test\ninclude_patterns: [\"app-*\"]\nsince: 0s\n",
			want: DockerConfig{
				Host:            "unix:///var/run/docker.sock",
				Hosts:           []DockerHostConfig{{Host: "ssh://coach_test"}},
				IncludePatterns: []string{"app-*"},
				Since:           "0s",
			},
		},
		{
			name: "supports host object overrides",
			data: "hosts:\n  - host: ssh://coach_test\n    include_patterns: [\"api-*\"]\n    since: 30s\n    aggregation:\n      flush_size: 5\n    dingtalk:\n      webhook_url: \"\"\n",
			want: DockerConfig{
				Hosts: []DockerHostConfig{
					{
						Host:            "ssh://coach_test",
						IncludePatterns: []string{"api-*"},
						Since:           stringPtr("30s"),
						Aggregation: &AggregationOverrideConfig{
							FlushSize: intPtr(5),
						},
						DingTalk: &DingTalkOverrideConfig{
							WebhookURL: stringPtr(""),
						},
					},
				},
			},
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var got DockerConfig
			if err := yaml.Unmarshal([]byte(tt.data), &got); err != nil {
				t.Fatalf("yaml.Unmarshal() error = %v", err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("yaml.Unmarshal() = %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestConfigResolveHostsAppliesHostOverrides(t *testing.T) {
	t.Parallel()

	cfg := defaultConfig()
	cfg.Docker.IncludePatterns = []string{"common-*"}
	cfg.Docker.Since = "10s"
	cfg.Aggregation.FlushSize = 20
	cfg.DingTalk.WebhookURL = "https://example.com/common"
	cfg.Docker.Hosts = []DockerHostConfig{
		{
			Host:            "ssh://prod-a",
			IncludePatterns: []string{"api-*"},
			Since:           stringPtr("1m"),
			Filters: &FilterOverrideConfig{
				WarnMatch: &WarnMatchOverrideConfig{
					ExcludeRegexps: []string{"noise"},
				},
			},
			Aggregation: &AggregationOverrideConfig{
				FlushSize: intPtr(5),
			},
			DingTalk: &DingTalkOverrideConfig{
				WebhookURL: stringPtr(""),
				AtAll:      boolPtr(true),
			},
			Storage: &StorageOverrideConfig{
				OutputDir: stringPtr("data/prod-a"),
			},
		},
		{
			Host: "ssh://prod-b",
		},
	}

	resolved, err := cfg.ResolveHosts()
	if err != nil {
		t.Fatalf("ResolveHosts() error = %v", err)
	}
	if len(resolved) != 2 {
		t.Fatalf("len(ResolveHosts()) = %d, want 2", len(resolved))
	}

	first := resolved[0].Config
	if resolved[0].Host != "ssh://prod-a" {
		t.Fatalf("ResolveHosts()[0].Host = %q, want %q", resolved[0].Host, "ssh://prod-a")
	}
	if !reflect.DeepEqual(first.Docker.IncludePatterns, []string{"api-*"}) {
		t.Fatalf("ResolveHosts()[0].Docker.IncludePatterns = %v, want %v", first.Docker.IncludePatterns, []string{"api-*"})
	}
	if first.Docker.Since != "1m" {
		t.Fatalf("ResolveHosts()[0].Docker.Since = %q, want %q", first.Docker.Since, "1m")
	}
	if !reflect.DeepEqual(first.Filters.WarnMatch.ExcludeRegexps, []string{"noise"}) {
		t.Fatalf("ResolveHosts()[0].Filters.WarnMatch.ExcludeRegexps = %v, want %v", first.Filters.WarnMatch.ExcludeRegexps, []string{"noise"})
	}
	if first.Aggregation.FlushSize != 5 {
		t.Fatalf("ResolveHosts()[0].Aggregation.FlushSize = %d, want %d", first.Aggregation.FlushSize, 5)
	}
	if first.DingTalk.WebhookURL != "" {
		t.Fatalf("ResolveHosts()[0].DingTalk.WebhookURL = %q, want empty string", first.DingTalk.WebhookURL)
	}
	if !first.DingTalk.AtAll {
		t.Fatal("ResolveHosts()[0].DingTalk.AtAll = false, want true")
	}
	if first.Storage.OutputDir != "data/prod-a" {
		t.Fatalf("ResolveHosts()[0].Storage.OutputDir = %q, want %q", first.Storage.OutputDir, "data/prod-a")
	}

	second := resolved[1].Config
	if second.Docker.Since != "10s" {
		t.Fatalf("ResolveHosts()[1].Docker.Since = %q, want %q", second.Docker.Since, "10s")
	}
	if second.Aggregation.FlushSize != 20 {
		t.Fatalf("ResolveHosts()[1].Aggregation.FlushSize = %d, want %d", second.Aggregation.FlushSize, 20)
	}
	if second.DingTalk.WebhookURL != "https://example.com/common" {
		t.Fatalf("ResolveHosts()[1].DingTalk.WebhookURL = %q, want %q", second.DingTalk.WebhookURL, "https://example.com/common")
	}
}

func TestLoadKeepsDockerDefaultsWhenUsingHostOverridesOnly(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := dir + "/config.yaml"
	data := []byte("docker:\n  hosts:\n    - host: ssh://prod-a\n      include_patterns:\n        - api-*\n")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatalf("os.WriteFile() error = %v", err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}

	if cfg.Docker.Since != "0s" {
		t.Fatalf("Load().Docker.Since = %q, want %q", cfg.Docker.Since, "0s")
	}

	resolved, err := cfg.ResolveHosts()
	if err != nil {
		t.Fatalf("ResolveHosts() error = %v", err)
	}
	if len(resolved) != 1 {
		t.Fatalf("len(ResolveHosts()) = %d, want 1", len(resolved))
	}
	if !reflect.DeepEqual(resolved[0].Config.Docker.IncludePatterns, []string{"api-*"}) {
		t.Fatalf("ResolveHosts()[0].Config.Docker.IncludePatterns = %v, want %v", resolved[0].Config.Docker.IncludePatterns, []string{"api-*"})
	}
}

func TestConfigValidateRejectsDuplicateResolvedHosts(t *testing.T) {
	t.Parallel()

	cfg := defaultConfig()
	cfg.Docker.Hosts = []DockerHostConfig{
		{Host: "ssh://prod-a"},
		{Host: " ssh://prod-a "},
	}

	err := cfg.Validate()
	if err == nil {
		t.Fatal("Validate() error = nil, want error")
	}
	if !strings.Contains(err.Error(), "duplicate docker host") {
		t.Fatalf("Validate() error = %v, want duplicate docker host", err)
	}
}

func TestDefaultConfigSetsDingTalkMaxEvents(t *testing.T) {
	t.Parallel()

	cfg := defaultConfig()
	if got := cfg.DingTalk.MaxEvents; got != 5 {
		t.Fatalf("defaultConfig().DingTalk.MaxEvents = %d, want 5", got)
	}
}

func TestConfigValidateRejectsNonPositiveDingTalkMaxEvents(t *testing.T) {
	t.Parallel()

	cfg := defaultConfig()
	cfg.DingTalk.MaxEvents = 0

	err := cfg.Validate()
	if err == nil {
		t.Fatal("Validate() error = nil, want error")
	}
	if !strings.Contains(err.Error(), "dingtalk.max_events") {
		t.Fatalf("Validate() error = %v, want dingtalk.max_events", err)
	}
}

func TestConfigValidateRejectsBlankWebhookWhenSecretConfigured(t *testing.T) {
	t.Parallel()

	cfg := defaultConfig()
	cfg.DingTalk.Secret = "secret"
	cfg.DingTalk.WebhookURL = "   "

	err := cfg.Validate()
	if err == nil {
		t.Fatal("Validate() error = nil, want error")
	}
	if !strings.Contains(err.Error(), "dingtalk.webhook_url must not be empty") {
		t.Fatalf("Validate() error = %v, want blank webhook validation error", err)
	}
}

func TestConfigValidateRejectsUnsupportedWebhookScheme(t *testing.T) {
	t.Parallel()

	cfg := defaultConfig()
	cfg.DingTalk.WebhookURL = "ftp://example.com/hook"

	err := cfg.Validate()
	if err == nil {
		t.Fatal("Validate() error = nil, want error")
	}
	if !strings.Contains(err.Error(), "dingtalk.webhook_url must use http or https") {
		t.Fatalf("Validate() error = %v, want webhook scheme validation error", err)
	}
}

func stringPtr(value string) *string {
	return &value
}

func intPtr(value int) *int {
	return &value
}

func boolPtr(value bool) *bool {
	return &value
}
