package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Project is the part of wsm's management API project the spike reads.
type Project struct {
	Slug      string `json:"slug"`
	LinuxUser string `json:"linux_user"`
	LinuxUID  int    `json:"linux_uid"`
	Image     string `json:"image"`
	CPUMillis int64  `json:"cpu_millis"`
	MemBytes  int64  `json:"mem_bytes"`
	PidsLimit int64  `json:"pids_limit"`
}

// Sandbox is the part of wsm's management API sandbox the spike reads.
type Sandbox struct {
	Project   string    `json:"project"`
	Slug      string    `json:"slug"`
	Status    string    `json:"status"`
	Pod       string    `json:"pod"`
	CreatedAt time.Time `json:"created_at"`
}

// Admin is wsm's management API as the server uses it. In P1 the tenant
// API lives inside the manager and calls the sandbox manager directly.
type Admin interface {
	Project(ctx context.Context, slug string) (Project, error)
	Sandboxes(ctx context.Context, project string) ([]Sandbox, error)
	CreateSandbox(ctx context.Context, project, slug string) (Sandbox, error)
	SandboxAction(ctx context.Context, project, slug, action string) error
	DeleteSandbox(ctx context.Context, project, slug string) error
}

// AdminClient talks to the management API socket.
type AdminClient struct {
	http *http.Client
}

const maxAdminResponse = 4 << 20

// NewAdminClient returns a client for the management API at socket.
func NewAdminClient(socket string) *AdminClient {
	return &AdminClient{http: &http.Client{
		Timeout: 3 * time.Minute,
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				var d net.Dialer
				return d.DialContext(ctx, "unix", socket)
			},
			MaxIdleConns:    4,
			IdleConnTimeout: 30 * time.Second,
		},
	}}
}

func (c *AdminClient) do(ctx context.Context, method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = strings.NewReader(string(b))
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
		return fmt.Errorf("%s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxAdminResponse+1))
	if err != nil {
		return fmt.Errorf("%s %s: %w", method, path, err)
	}
	if len(data) > maxAdminResponse {
		return fmt.Errorf("%s %s: response too large", method, path)
	}
	if resp.StatusCode >= 300 {
		var e struct {
			Error string `json:"error"`
		}
		msg := http.StatusText(resp.StatusCode)
		if json.Unmarshal(data, &e) == nil && e.Error != "" {
			msg = e.Error
		}
		if len(msg) > 256 {
			msg = msg[:256]
		}
		return fmt.Errorf("%s %s: status %d: %s", method, path, resp.StatusCode, msg)
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("%s %s: decode: %w", method, path, err)
	}
	return nil
}

func (c *AdminClient) Project(ctx context.Context, slug string) (Project, error) {
	var ps []Project
	if err := c.do(ctx, http.MethodGet, "/v1/projects", nil, &ps); err != nil {
		return Project{}, err
	}
	for _, p := range ps {
		if p.Slug == slug {
			return p, nil
		}
	}
	return Project{}, fmt.Errorf("project %s not found", slug)
}

func (c *AdminClient) Sandboxes(ctx context.Context, project string) ([]Sandbox, error) {
	var all []Sandbox
	if err := c.do(ctx, http.MethodGet, "/v1/sandboxes", nil, &all); err != nil {
		return nil, err
	}
	var out []Sandbox
	for _, sb := range all {
		if sb.Project == project {
			out = append(out, sb)
		}
	}
	return out, nil
}

func sandboxPath(project, slug string) string {
	return "/v1/projects/" + url.PathEscape(project) + "/sandboxes/" + url.PathEscape(slug)
}

func (c *AdminClient) CreateSandbox(ctx context.Context, project, slug string) (Sandbox, error) {
	var sb Sandbox
	err := c.do(ctx, http.MethodPost, "/v1/projects/"+url.PathEscape(project)+"/sandboxes",
		map[string]string{"slug": slug}, &sb)
	return sb, err
}

func (c *AdminClient) SandboxAction(ctx context.Context, project, slug, action string) error {
	if action != "start" && action != "stop" {
		return fmt.Errorf("unknown action %q", action)
	}
	return c.do(ctx, http.MethodPost, sandboxPath(project, slug)+"/"+action, nil, nil)
}

func (c *AdminClient) DeleteSandbox(ctx context.Context, project, slug string) error {
	return c.do(ctx, http.MethodDelete, sandboxPath(project, slug), nil, nil)
}
