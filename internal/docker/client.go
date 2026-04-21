package docker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

type Client struct {
	baseURL    string
	httpClient *http.Client
}

// APIError 表示 docker daemon 返回的非 2xx 响应。
// 业务代码应优先用 errors.As 解出 *APIError 后再判断 StatusCode，
// 避免依赖 Error() 文本格式。
type APIError struct {
	Method     string
	Path       string
	Status     string
	StatusCode int
	Body       string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("docker api %s %s returned %s: %s", e.Method, e.Path, e.Status, e.Body)
}

type ContainerListOptions struct {
	All bool
}

type ContainerLogsOptions struct {
	ShowStdout bool
	ShowStderr bool
	Since      string
	Timestamps bool
	Follow     bool
}

type EventsOptions struct {
	Since   string
	Until   string
	Filters map[string][]string
}

type ContainerSummary struct {
	ID    string   `json:"Id"`
	Names []string `json:"Names"`
}

type ContainerJSON struct {
	ID    string         `json:"Id"`
	Name  string         `json:"Name"`
	State ContainerState `json:"State"`
}

type ContainerState struct {
	Status     string `json:"Status"`
	Running    bool   `json:"Running"`
	Restarting bool   `json:"Restarting"`
	ExitCode   int    `json:"ExitCode"`
}

type EventActor struct {
	ID         string            `json:"ID"`
	Attributes map[string]string `json:"Attributes"`
}

type EventMessage struct {
	Status string     `json:"status,omitempty"`
	ID     string     `json:"id,omitempty"`
	Type   string     `json:"Type"`
	Action string     `json:"Action"`
	Actor  EventActor `json:"Actor"`
	Time   int64      `json:"time,omitempty"`
}

const dockerShortRequestTimeout = 10 * time.Second
const dockerEventBufferSize = 64

func NewClient(host string) (*Client, error) {
	resolvedHost := host
	if resolvedHost == "" {
		resolvedHost = os.Getenv("DOCKER_HOST")
	}
	if resolvedHost == "" {
		resolvedHost = "unix:///var/run/docker.sock"
	}

	transport, baseURL, err := newHTTPTransport(resolvedHost)
	if err != nil {
		return nil, err
	}

	return &Client{
		baseURL: baseURL,
		httpClient: &http.Client{
			Transport: transport,
			Timeout:   0,
		},
	}, nil
}

func newHTTPTransport(host string) (*http.Transport, string, error) {
	parsed, err := url.Parse(host)
	if err != nil {
		return nil, "", fmt.Errorf("parse docker host: %w", err)
	}

	transport := &http.Transport{}
	baseURL := host

	switch parsed.Scheme {
	case "unix":
		socketPath := parsed.Path
		transport.DialContext = func(ctx context.Context, _, _ string) (net.Conn, error) {
			var dialer net.Dialer
			return dialer.DialContext(ctx, "unix", socketPath)
		}
		baseURL = "http://docker"
	case "tcp":
		baseURL = "http://" + strings.TrimPrefix(host, "tcp://")
	case "http", "https":
		baseURL = host
	case "ssh":
		helper, err := newSSHConnectionHelper(host)
		if err != nil {
			return nil, "", err
		}
		transport.DialContext = helper.DialContext
		baseURL = helper.BaseURL
	default:
		return nil, "", fmt.Errorf("unsupported docker host scheme: %s", parsed.Scheme)
	}

	return transport, baseURL, nil
}

func (c *Client) Close() error {
	c.httpClient.CloseIdleConnections()
	return nil
}

func (c *Client) ContainerList(ctx context.Context, options ContainerListOptions) ([]ContainerSummary, error) {
	ctx, cancel := withRequestTimeout(ctx, dockerShortRequestTimeout)
	defer cancel()

	query := url.Values{}
	if options.All {
		query.Set("all", "1")
	}

	resp, err := c.do(ctx, http.MethodGet, "/containers/json", query)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var containers []ContainerSummary
	if err := json.NewDecoder(resp.Body).Decode(&containers); err != nil {
		return nil, fmt.Errorf("decode containers: %w", err)
	}
	return containers, nil
}

func (c *Client) ContainerInspect(ctx context.Context, containerID string) (ContainerJSON, error) {
	if strings.TrimSpace(containerID) == "" {
		return ContainerJSON{}, fmt.Errorf("container id is empty")
	}

	ctx, cancel := withRequestTimeout(ctx, dockerShortRequestTimeout)
	defer cancel()

	resp, err := c.do(ctx, http.MethodGet, "/containers/"+containerID+"/json", nil)
	if err != nil {
		return ContainerJSON{}, err
	}
	defer resp.Body.Close()

	var container ContainerJSON
	if err := json.NewDecoder(resp.Body).Decode(&container); err != nil {
		return ContainerJSON{}, fmt.Errorf("decode container inspect: %w", err)
	}
	return container, nil
}

func (c *Client) ContainerLogs(ctx context.Context, containerID string, options ContainerLogsOptions) (io.ReadCloser, error) {
	if strings.TrimSpace(containerID) == "" {
		return nil, fmt.Errorf("container id is empty")
	}

	query := url.Values{}
	if options.ShowStdout {
		query.Set("stdout", "1")
	}
	if options.ShowStderr {
		query.Set("stderr", "1")
	}
	if options.Since != "" {
		query.Set("since", options.Since)
	}
	if options.Timestamps {
		query.Set("timestamps", "1")
	}
	if options.Follow {
		query.Set("follow", "1")
	}

	resp, err := c.do(ctx, http.MethodGet, "/containers/"+containerID+"/logs", query)
	if err != nil {
		return nil, err
	}
	return resp.Body, nil
}

func (c *Client) Events(ctx context.Context, options EventsOptions) (<-chan EventMessage, <-chan error) {
	msgCh := make(chan EventMessage, dockerEventBufferSize)
	errCh := make(chan error, 1)

	go func() {
		defer close(msgCh)
		defer close(errCh)

		query := url.Values{}
		if options.Since != "" {
			query.Set("since", options.Since)
		}
		if options.Until != "" {
			query.Set("until", options.Until)
		}
		if len(options.Filters) > 0 {
			payload, err := json.Marshal(options.Filters)
			if err != nil {
				errCh <- fmt.Errorf("marshal event filters: %w", err)
				return
			}
			query.Set("filters", string(payload))
		}

		resp, err := c.do(ctx, http.MethodGet, "/events", query)
		if err != nil {
			errCh <- err
			return
		}
		defer resp.Body.Close()

		decoder := json.NewDecoder(resp.Body)
		for {
			var event EventMessage
			if err := decoder.Decode(&event); err != nil {
				if err == io.EOF || errors.Is(err, context.Canceled) || errors.Is(ctx.Err(), context.Canceled) {
					return
				}
				errCh <- err
				return
			}

			select {
			case <-ctx.Done():
				return
			case msgCh <- event:
			}
		}
	}()

	return msgCh, errCh
}

func (c *Client) do(ctx context.Context, method, path string, query url.Values) (*http.Response, error) {
	reqURL := c.baseURL + path
	if encoded := query.Encode(); encoded != "" {
		reqURL += "?" + encoded
	}

	req, err := http.NewRequestWithContext(ctx, method, reqURL, nil)
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("docker api request failed: %w", err)
	}
	if resp.StatusCode >= 300 {
		defer resp.Body.Close()
		body, readErr := io.ReadAll(io.LimitReader(resp.Body, 4096))
		if readErr != nil {
			body = nil
		}
		return nil, &APIError{
			Method:     method,
			Path:       path,
			Status:     resp.Status,
			StatusCode: resp.StatusCode,
			Body:       strings.TrimSpace(string(body)),
		}
	}
	return resp, nil
}

func FormatSince(ts time.Time) string {
	if ts.IsZero() {
		return ""
	}
	return strconv.FormatInt(ts.UTC().Unix(), 10)
}

func withRequestTimeout(ctx context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	if _, hasDeadline := ctx.Deadline(); hasDeadline || timeout <= 0 {
		return ctx, func() {}
	}
	return context.WithTimeout(ctx, timeout)
}
