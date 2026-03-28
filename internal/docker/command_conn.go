package docker

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"os/exec"
	"strings"
	"sync"
	"time"
)

func newCommandConn(ctx context.Context, cmd string, args ...string) (net.Conn, error) {
	command := exec.CommandContext(ctx, cmd, args...)

	stdin, err := command.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := command.StdoutPipe()
	if err != nil {
		return nil, err
	}

	conn := &commandConn{
		cmd:        command,
		stdin:      stdin,
		stdout:     stdout,
		localAddr:  commandAddr("command-local"),
		remoteAddr: commandAddr("command-remote"),
	}
	command.Stderr = &conn.stderr

	if err := command.Start(); err != nil {
		return nil, err
	}
	return conn, nil
}

type commandConn struct {
	cmd        *exec.Cmd
	stdin      io.WriteCloser
	stdout     io.ReadCloser
	stderr     bytes.Buffer
	waitOnce   sync.Once
	waitErr    error
	closeOnce  sync.Once
	localAddr  net.Addr
	remoteAddr net.Addr
}

func (c *commandConn) Read(p []byte) (int, error) {
	n, err := c.stdout.Read(p)
	if err == io.EOF {
		return n, c.handleEOF(err)
	}
	return n, err
}

func (c *commandConn) Write(p []byte) (int, error) {
	n, err := c.stdin.Write(p)
	if err == io.EOF {
		return n, c.handleEOF(err)
	}
	return n, err
}

func (c *commandConn) Close() error {
	var closeErr error
	c.closeOnce.Do(func() {
		if err := c.stdin.Close(); err != nil && closeErr == nil {
			closeErr = err
		}
		if err := c.stdout.Close(); err != nil && closeErr == nil {
			closeErr = err
		}
		_ = c.cmd.Process.Kill()
		_ = c.wait()
	})
	return closeErr
}

func (c *commandConn) LocalAddr() net.Addr  { return c.localAddr }
func (c *commandConn) RemoteAddr() net.Addr { return c.remoteAddr }

func (c *commandConn) SetDeadline(time.Time) error      { return nil }
func (c *commandConn) SetReadDeadline(time.Time) error  { return nil }
func (c *commandConn) SetWriteDeadline(time.Time) error { return nil }

func (c *commandConn) handleEOF(err error) error {
	if err != io.EOF {
		return err
	}
	waitErr := c.wait()
	if waitErr == nil {
		return err
	}
	return fmt.Errorf("%w: %s", waitErr, strings.TrimSpace(c.stderr.String()))
}

func (c *commandConn) wait() error {
	c.waitOnce.Do(func() {
		c.waitErr = c.cmd.Wait()
	})
	return c.waitErr
}

type commandAddr string

func (a commandAddr) Network() string { return "command" }
func (a commandAddr) String() string  { return string(a) }
