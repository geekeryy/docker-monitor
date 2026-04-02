package docker

import "testing"

func TestParseSSHSpec(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		raw     string
		want    sshSpec
		wantErr bool
	}{
		{
			name: "parses ssh alias host",
			raw:  "ssh://coach_test",
			want: sshSpec{Host: "coach_test"},
		},
		{
			name: "parses user port and socket path",
			raw:  "ssh://root@example.com:2222/var/run/docker.sock",
			want: sshSpec{User: "root", Host: "example.com", Port: "2222", Path: "/var/run/docker.sock"},
		},
		{
			name:    "rejects password in url",
			raw:     "ssh://root:secret@example.com",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := parseSSHSpec(tt.raw)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("parseSSHSpec(%q) error = nil, want error", tt.raw)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseSSHSpec(%q) error = %v", tt.raw, err)
			}
			if *got != tt.want {
				t.Fatalf("parseSSHSpec(%q) = %+v, want %+v", tt.raw, *got, tt.want)
			}
		})
	}
}

func TestDockerDialStdioArgs(t *testing.T) {
	t.Parallel()

	if got := dockerDialStdioArgs(""); len(got) != 2 || got[0] != "system" || got[1] != "dial-stdio" {
		t.Fatalf("dockerDialStdioArgs(\"\") = %v, want [system dial-stdio]", got)
	}

	got := dockerDialStdioArgs("/var/run/docker.sock")
	wantFirst := "--host=unix:///var/run/docker.sock"
	if len(got) != 3 || got[0] != wantFirst || got[1] != "system" || got[2] != "dial-stdio" {
		t.Fatalf("dockerDialStdioArgs(socket) = %v, want [%s system dial-stdio]", got, wantFirst)
	}
}

func TestSSHSpecCommandAddsKeepaliveOptions(t *testing.T) {
	t.Parallel()

	spec := sshSpec{
		User: "root",
		Host: "example.com",
		Port: "2222",
	}

	got, err := spec.command("docker", "system", "dial-stdio")
	if err != nil {
		t.Fatalf("spec.command() error = %v", err)
	}

	want := []string{
		"-l", "root",
		"-p", "2222",
		"-o", "ConnectTimeout=30",
		"-o", "ServerAliveInterval=30",
		"-o", "ServerAliveCountMax=3",
		"-T", "--", "example.com",
		"'docker' 'system' 'dial-stdio'",
	}

	if len(got) != len(want) {
		t.Fatalf("len(spec.command()) = %d, want %d; got %v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("spec.command()[%d] = %q, want %q; full=%v", i, got[i], want[i], got)
		}
	}
}
