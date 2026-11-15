package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"

	"github.com/coder/websocket"
)

// maxResponseBody caps a response body read by the client.
const maxResponseBody = 4 << 20

// Client talks to the manager's API.
type Client struct {
	http *http.Client
}

// NewClient returns a client for the API socket at path.
func NewClient(path string) *Client {
	return NewClientDialer(func(ctx context.Context) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "unix", path)
	})
}

// NewClientDialer returns a client that connects with dial.
func NewClientDialer(dial func(ctx context.Context) (net.Conn, error)) *Client {
	return &Client{http: &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			conn, err := dial(ctx)
			if err != nil {
				return nil, fmt.Errorf("connect to wsm: %w", err)
			}
			return conn, nil
		},
		DisableCompression: true,
	}}}
}

func (c *Client) do(ctx context.Context, method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, "http://wsm"+path, body)
	if err != nil {
		return err
	}
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBody+1))
	if err != nil {
		return err
	}
	if len(data) > maxResponseBody {
		return fmt.Errorf("response exceeds %d bytes", maxResponseBody)
	}
	if resp.StatusCode >= 300 {
		var e Error
		if json.Unmarshal(data, &e) == nil && e.Error != "" {
			return fmt.Errorf("%s", e.Error)
		}
		return fmt.Errorf("%s %s: %s", method, path, resp.Status)
	}
	if out != nil {
		if err := json.Unmarshal(data, out); err != nil {
			return fmt.Errorf("decode response: %w", err)
		}
	}
	return nil
}

func sandboxPath(project, sandbox string) string {
	return "/v1/projects/" + url.PathEscape(project) + "/sandboxes/" + url.PathEscape(sandbox)
}

// Projects lists all projects.
func (c *Client) Projects(ctx context.Context) ([]Project, error) {
	var out []Project
	err := c.do(ctx, http.MethodGet, "/v1/projects", nil, &out)
	return out, err
}

// CreateProject creates a project.
func (c *Client) CreateProject(ctx context.Context, req CreateProjectRequest) (Project, error) {
	var out Project
	err := c.do(ctx, http.MethodPost, "/v1/projects", req, &out)
	return out, err
}

// ProjectImage builds or pulls a project's image now.
func (c *Client) ProjectImage(ctx context.Context, project string) (ProjectImage, error) {
	var out ProjectImage
	err := c.do(ctx, http.MethodPost, "/v1/projects/"+url.PathEscape(project)+"/image", nil, &out)
	return out, err
}

// DeployKey returns the public deploy key of a project with a repository.
func (c *Client) DeployKey(ctx context.Context, project string) (DeployKey, error) {
	var out DeployKey
	err := c.do(ctx, http.MethodGet, "/v1/projects/"+url.PathEscape(project)+"/key", nil, &out)
	return out, err
}

// DeleteProject deletes a project with all its sandboxes.
func (c *Client) DeleteProject(ctx context.Context, project string) error {
	return c.do(ctx, http.MethodDelete, "/v1/projects/"+url.PathEscape(project), nil, nil)
}

// Blocks lists the ceilings of a project.
func (c *Client) Blocks(ctx context.Context, project string) ([]Block, error) {
	var out []Block
	err := c.do(ctx, http.MethodGet, blocksPath(project), nil, &out)
	return out, err
}

// SetBlock sets the ceiling of a project for a block and waits until every
// instance it names is ready.
func (c *Client) SetBlock(ctx context.Context, project, block string, req SetBlockRequest) (Block, error) {
	var out Block
	err := c.do(ctx, http.MethodPut, blocksPath(project)+"/"+url.PathEscape(block), req, &out)
	return out, err
}

// RemoveBlock removes the ceiling of a project for a block.
func (c *Client) RemoveBlock(ctx context.Context, project, block string) error {
	return c.do(ctx, http.MethodDelete, blocksPath(project)+"/"+url.PathEscape(block), nil, nil)
}

// Services lists the shared service instances with their live state.
func (c *Client) Services(ctx context.Context) ([]Service, error) {
	var out []Service
	err := c.do(ctx, http.MethodGet, "/v1/services", nil, &out)
	return out, err
}

// Sandboxes lists sandboxes, of one project when project is not empty.
func (c *Client) Sandboxes(ctx context.Context, project string) ([]Sandbox, error) {
	path := "/v1/sandboxes"
	if project != "" {
		path += "?" + url.Values{"project": {project}}.Encode()
	}
	var out []Sandbox
	err := c.do(ctx, http.MethodGet, path, nil, &out)
	return out, err
}

// CreateSandbox creates a sandbox in a project.
func (c *Client) CreateSandbox(ctx context.Context, project, sandbox string) (Sandbox, error) {
	var out Sandbox
	err := c.do(ctx, http.MethodPost, "/v1/projects/"+url.PathEscape(project)+"/sandboxes",
		CreateSandboxRequest{Slug: sandbox}, &out)
	return out, err
}

// StartSandbox starts a sandbox.
func (c *Client) StartSandbox(ctx context.Context, project, sandbox string) error {
	return c.do(ctx, http.MethodPost, sandboxPath(project, sandbox)+"/start", nil, nil)
}

// StopSandbox stops a sandbox.
func (c *Client) StopSandbox(ctx context.Context, project, sandbox string) error {
	return c.do(ctx, http.MethodPost, sandboxPath(project, sandbox)+"/stop", nil, nil)
}

// DeleteSandbox deletes a sandbox.
func (c *Client) DeleteSandbox(ctx context.Context, project, sandbox string) error {
	return c.do(ctx, http.MethodDelete, sandboxPath(project, sandbox), nil, nil)
}

// Terminal opens a terminal WebSocket to a running sandbox.
func (c *Client) Terminal(ctx context.Context, project, sandbox string, rows, cols int) (*websocket.Conn, error) {
	q := url.Values{"rows": {strconv.Itoa(rows)}, "cols": {strconv.Itoa(cols)}}
	ws, resp, err := websocket.Dial(ctx, "ws://wsm"+sandboxPath(project, sandbox)+"/terminal?"+q.Encode(),
		&websocket.DialOptions{HTTPClient: c.http})
	if err != nil {
		if resp != nil && resp.Body != nil {
			var e Error
			data, _ := io.ReadAll(io.LimitReader(resp.Body, maxResponseBody))
			if json.Unmarshal(data, &e) == nil && e.Error != "" {
				return nil, fmt.Errorf("%s", e.Error)
			}
		}
		return nil, err
	}
	return ws, nil
}
