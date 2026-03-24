package config

import (
	"reflect"
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
				Host:  "unix:///var/run/docker.sock",
				Hosts: []string{"ssh://coach_test", "unix:///var/run/docker.sock", " tcp://127.0.0.1:2375 "},
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
				Hosts:           []string{"unix:///var/run/docker.sock", "ssh://coach_test"},
				IncludePatterns: []string{"app-*"},
				Since:           "0s",
			},
		},
		{
			name: "merges host and hosts",
			data: "host: unix:///var/run/docker.sock\nhosts:\n  - ssh://coach_test\ninclude_patterns: [\"app-*\"]\nsince: 0s\n",
			want: DockerConfig{
				Host:            "unix:///var/run/docker.sock",
				Hosts:           []string{"ssh://coach_test"},
				IncludePatterns: []string{"app-*"},
				Since:           "0s",
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
