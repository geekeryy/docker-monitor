package docker

import (
	"context"
	"fmt"
	"net"
	"net/url"
	"os/exec"
	"strconv"
	"strings"
)

type sshConnectionHelper struct {
	BaseURL     string
	DialContext func(ctx context.Context, network, addr string) (net.Conn, error)
}

func newSSHConnectionHelper(host string) (*sshConnectionHelper, error) {
	if err := validateSSHBinary(); err != nil {
		return nil, err
	}

	spec, err := parseSSHSpec(host)
	if err != nil {
		return nil, err
	}

	sshArgs, err := spec.command("docker", dockerDialStdioArgs(spec.Path)...)
	if err != nil {
		return nil, err
	}

	return &sshConnectionHelper{
		BaseURL: "http://docker.example.com",
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			return newCommandConn(ctx, "ssh", sshArgs...)
		},
	}, nil
}

func dockerDialStdioArgs(socketPath string) []string {
	if strings.Trim(socketPath, "/") == "" {
		return []string{"system", "dial-stdio"}
	}
	return []string{"--host=unix://" + socketPath, "system", "dial-stdio"}
}

type sshSpec struct {
	User string
	Host string
	Port string
	Path string
}

func parseSSHSpec(raw string) (*sshSpec, error) {
	parsed, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("parse ssh docker host: %w", err)
	}
	if parsed.Scheme != "ssh" {
		return nil, fmt.Errorf("unsupported ssh docker host scheme: %s", parsed.Scheme)
	}
	if parsed.Hostname() == "" {
		return nil, fmt.Errorf("invalid ssh docker host: empty hostname")
	}
	if parsed.RawQuery != "" || parsed.Fragment != "" {
		return nil, fmt.Errorf("invalid ssh docker host: query and fragment are not supported")
	}
	if parsed.User != nil {
		if _, ok := parsed.User.Password(); ok {
			return nil, fmt.Errorf("invalid ssh docker host: password is not supported")
		}
	}

	return &sshSpec{
		User: parsed.User.Username(),
		Host: parsed.Hostname(),
		Port: parsed.Port(),
		Path: parsed.Path,
	}, nil
}

func (s *sshSpec) command(remoteCmd string, remoteArgs ...string) ([]string, error) {
	args := make([]string, 0, 8+len(remoteArgs))
	if s.User != "" {
		args = append(args, "-l", s.User)
	}
	if s.Port != "" {
		if _, err := strconv.Atoi(s.Port); err != nil {
			return nil, fmt.Errorf("invalid ssh port %q: %w", s.Port, err)
		}
		args = append(args, "-p", s.Port)
	}
	args = append(args,
		"-o", "ConnectTimeout=30",
		"-o", "ServerAliveInterval=30",
		"-o", "ServerAliveCountMax=3",
		"-T", "--", s.Host,
	)
	args = append(args, shellJoin(append([]string{remoteCmd}, remoteArgs...)...))
	return args, nil
}

func shellJoin(parts ...string) string {
	quoted := make([]string, 0, len(parts))
	for _, part := range parts {
		quoted = append(quoted, shellQuote(part))
	}
	return strings.Join(quoted, " ")
}

func shellQuote(value string) string {
	if value == "" {
		return "''"
	}
	return "'" + strings.ReplaceAll(value, "'", `'"'"'`) + "'"
}

func validateSSHBinary() error {
	if _, err := exec.LookPath("ssh"); err != nil {
		return fmt.Errorf("ssh binary not found: %w", err)
	}
	return nil
}
