package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

// apiVersion is the libpod API version in every request path.
const apiVersion = "v5.0.0"

// maxResponse caps every JSON response body read from Podman.
const maxResponse = 1 << 20

// Client is a thin libpod REST client over a Podman unix socket, built on the
// standard library only.
type Client struct {
	socket string
	http   *http.Client
}

// NewClient returns a client for the Podman API socket at path.
func NewClient(path string) *Client {
	c := &Client{socket: path}
	c.http = &http.Client{Transport: &http.Transport{
		DialContext:        c.dial,
		DisableCompression: true,
	}}
	return c
}

func (c *Client) dial(ctx context.Context, _, _ string) (net.Conn, error) {
	var d net.Dialer
	conn, err := d.DialContext(ctx, "unix", c.socket)
	if err != nil {
		return nil, fmt.Errorf("dial podman socket: %w", err)
	}
	return conn, nil
}

// APIError is an error response from Podman.
type APIError struct {
	Status  int    `json:"response"`
	Message string `json:"message"`
	Cause   string `json:"cause"`
}

func (e *APIError) Error() string {
	return fmt.Sprintf("podman api: status %d: %s", e.Status, e.Message)
}

// IsNotFound reports whether err is a 404 from Podman.
func IsNotFound(err error) bool {
	var apiErr *APIError
	return errors.As(err, &apiErr) && apiErr.Status == http.StatusNotFound
}

func parseAPIError(status int, body []byte) error {
	apiErr := &APIError{}
	if err := json.Unmarshal(body, apiErr); err != nil || apiErr.Message == "" {
		apiErr.Message = http.StatusText(status)
	}
	apiErr.Status = status
	return apiErr
}

// readLimited reads all of r, failing if it holds more than limit bytes.
func readLimited(r io.Reader, limit int64) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(r, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("response exceeds %d bytes", limit)
	}
	return data, nil
}

func endpoint(path string, q url.Values) string {
	u := "http://podman/" + apiVersion + "/libpod" + path
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	return u
}

func encodeBody(in any) (io.Reader, error) {
	if in == nil {
		return nil, nil
	}
	b, err := json.Marshal(in)
	if err != nil {
		return nil, fmt.Errorf("encode request: %w", err)
	}
	return bytes.NewReader(b), nil
}

func (c *Client) do(ctx context.Context, method, path string, q url.Values, in, out any) error {
	body, err := encodeBody(in)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, method, endpoint(path, q), body)
	if err != nil {
		return fmt.Errorf("%s %s: %w", method, path, err)
	}
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("%s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	data, err := readLimited(resp.Body, maxResponse)
	if err != nil {
		return fmt.Errorf("%s %s: %w", method, path, err)
	}
	if resp.StatusCode >= 300 && resp.StatusCode != http.StatusNotModified {
		return fmt.Errorf("%s %s: %w", method, path, parseAPIError(resp.StatusCode, data))
	}
	if out == nil {
		return nil
	}
	if raw, ok := out.(*[]byte); ok {
		*raw = data
		return nil
	}
	if err := json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("%s %s: decode response: %w", method, path, err)
	}
	return nil
}

// Namespace is a libpod namespace setting, e.g. {"nsmode": "keep-id"}.
type Namespace struct {
	Mode  string `json:"nsmode"`
	Value string `json:"value,omitempty"`
}

// PodSpec is the subset of libpod's PodSpecGenerator the spike uses.
type PodSpec struct {
	Name   string     `json:"name"`
	Userns *Namespace `json:"userns,omitempty"`
	Netns  *Namespace `json:"netns,omitempty"`
}

// Resources is the subset of the OCI LinuxResources the spike sets.
type Resources struct {
	CPU    *CPULimit    `json:"cpu,omitempty"`
	Memory *MemoryLimit `json:"memory,omitempty"`
	Pids   *PidsLimit   `json:"pids,omitempty"`
}

// CPULimit is a CFS quota per period, in microseconds.
type CPULimit struct {
	Quota  int64  `json:"quota"`
	Period uint64 `json:"period"`
}

// MemoryLimit is a memory limit in bytes.
type MemoryLimit struct {
	Limit int64 `json:"limit"`
}

// PidsLimit is the maximum number of processes.
type PidsLimit struct {
	Limit int64 `json:"limit"`
}

// ContainerSpec is the subset of libpod's SpecGenerator the spike uses.
// There is deliberately no field for mounts or volumes.
type ContainerSpec struct {
	Name            string     `json:"name"`
	Image           string     `json:"image"`
	Pod             string     `json:"pod,omitempty"`
	User            string     `json:"user,omitempty"`
	Command         []string   `json:"command,omitempty"`
	Init            bool       `json:"init,omitempty"`
	CapDrop         []string   `json:"cap_drop,omitempty"`
	NoNewPrivileges bool       `json:"no_new_privileges,omitempty"`
	Resources       *Resources `json:"resource_limits,omitempty"`
}

type idResponse struct {
	ID string `json:"Id"`
}

// Version returns the Podman server version.
func (c *Client) Version(ctx context.Context) (string, error) {
	var v struct {
		Version string `json:"Version"`
	}
	if err := c.do(ctx, http.MethodGet, "/version", nil, nil, &v); err != nil {
		return "", err
	}
	return v.Version, nil
}

// PodCreate creates a pod and returns its ID.
func (c *Client) PodCreate(ctx context.Context, spec PodSpec) (string, error) {
	var r idResponse
	if err := c.do(ctx, http.MethodPost, "/pods/create", nil, spec, &r); err != nil {
		return "", err
	}
	return r.ID, nil
}

// PodStart starts every container in a pod.
func (c *Client) PodStart(ctx context.Context, name string) error {
	return c.do(ctx, http.MethodPost, "/pods/"+url.PathEscape(name)+"/start", nil, nil, nil)
}

// PodRemove force-removes a pod and its containers.
func (c *Client) PodRemove(ctx context.Context, name string) error {
	q := url.Values{"force": {"true"}}
	return c.do(ctx, http.MethodDelete, "/pods/"+url.PathEscape(name), q, nil, nil)
}

// PodInspectRaw returns the raw inspect JSON of a pod.
func (c *Client) PodInspectRaw(ctx context.Context, name string) ([]byte, error) {
	var raw []byte
	err := c.do(ctx, http.MethodGet, "/pods/"+url.PathEscape(name)+"/json", nil, nil, &raw)
	return raw, err
}

// ContainerCreate creates a container and returns its ID.
func (c *Client) ContainerCreate(ctx context.Context, spec ContainerSpec) (string, error) {
	var r idResponse
	if err := c.do(ctx, http.MethodPost, "/containers/create", nil, spec, &r); err != nil {
		return "", err
	}
	return r.ID, nil
}

// ContainerInspect is the subset of libpod's container inspect output the
// spike checks.
type ContainerInspect struct {
	State struct {
		Pid     int  `json:"Pid"`
		Running bool `json:"Running"`
	} `json:"State"`
	Mounts     []json.RawMessage `json:"Mounts"`
	HostConfig struct {
		Privileged  bool     `json:"Privileged"`
		UsernsMode  string   `json:"UsernsMode"`
		NetworkMode string   `json:"NetworkMode"`
		CapDrop     []string `json:"CapDrop"`
		SecurityOpt []string `json:"SecurityOpt"`
		CPUQuota    int64    `json:"CpuQuota"`
		CPUPeriod   uint64   `json:"CpuPeriod"`
		Memory      int64    `json:"Memory"`
		PidsLimit   int64    `json:"PidsLimit"`
	} `json:"HostConfig"`
}

// ContainerInspect returns the parsed and the raw inspect output.
func (c *Client) ContainerInspect(ctx context.Context, name string) (ContainerInspect, []byte, error) {
	var raw []byte
	var ci ContainerInspect
	if err := c.do(ctx, http.MethodGet, "/containers/"+url.PathEscape(name)+"/json", nil, nil, &raw); err != nil {
		return ci, nil, err
	}
	if err := json.Unmarshal(raw, &ci); err != nil {
		return ci, raw, fmt.Errorf("decode container inspect: %w", err)
	}
	return ci, raw, nil
}

// ExecConfig is the body of an exec create request.
type ExecConfig struct {
	AttachStdin  bool     `json:"AttachStdin"`
	AttachStdout bool     `json:"AttachStdout"`
	AttachStderr bool     `json:"AttachStderr"`
	Tty          bool     `json:"Tty"`
	Cmd          []string `json:"Cmd"`
	Env          []string `json:"Env,omitempty"`
}

// ExecCreate sets up an exec session in a running container.
func (c *Client) ExecCreate(ctx context.Context, container string, cfg ExecConfig) (string, error) {
	var r idResponse
	if err := c.do(ctx, http.MethodPost, "/containers/"+url.PathEscape(container)+"/exec", nil, cfg, &r); err != nil {
		return "", err
	}
	return r.ID, nil
}

type execStart struct {
	Detach bool `json:"Detach"`
	Tty    bool `json:"Tty"`
	Height int  `json:"h,omitempty"`
	Width  int  `json:"w,omitempty"`
}

// ExecStart starts an exec session and returns the hijacked connection.
// With a TTY the stream is raw; without one it is multiplexed (see demux).
func (c *Client) ExecStart(ctx context.Context, id string, tty bool, rows, cols int) (io.ReadWriteCloser, error) {
	body := execStart{Tty: tty}
	if tty {
		body.Height, body.Width = rows, cols
	}
	return c.hijack(ctx, "/exec/"+url.PathEscape(id)+"/start", body)
}

// ExecResize changes the TTY size of a running exec session.
func (c *Client) ExecResize(ctx context.Context, id string, rows, cols int) error {
	q := url.Values{"h": {strconv.Itoa(rows)}, "w": {strconv.Itoa(cols)}}
	return c.do(ctx, http.MethodPost, "/exec/"+url.PathEscape(id)+"/resize", q, nil, nil)
}

// ExecState is the subset of exec inspect output the spike reads.
type ExecState struct {
	Running  bool `json:"Running"`
	ExitCode int  `json:"ExitCode"`
}

// ExecInspect returns the state of an exec session.
func (c *Client) ExecInspect(ctx context.Context, id string) (ExecState, error) {
	var s ExecState
	err := c.do(ctx, http.MethodGet, "/exec/"+url.PathEscape(id)+"/json", nil, nil, &s)
	return s, err
}

// hijackedConn is the raw connection after Podman switched protocols. Reads
// go through the bufio.Reader that parsed the response head, so bytes it
// already buffered are not lost.
type hijackedConn struct {
	net.Conn
	r *bufio.Reader
}

func (h *hijackedConn) Read(p []byte) (int, error) { return h.r.Read(p) }

// hijack sends a POST and returns the connection once Podman answers with
// 101 Switching Protocols or 200 OK, after which the stream starts.
func (c *Client) hijack(ctx context.Context, path string, in any) (io.ReadWriteCloser, error) {
	body, err := encodeBody(in)
	if err != nil {
		return nil, err
	}
	conn, err := c.dial(ctx, "", "")
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint(path, nil), body)
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("POST %s: %w", path, err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", "tcp")
	hc, err := upgrade(conn, req)
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("POST %s: %w", path, err)
	}
	return hc, nil
}

// upgrade writes req to conn and reads the response head.
func upgrade(conn net.Conn, req *http.Request) (*hijackedConn, error) {
	if deadline, ok := req.Context().Deadline(); ok {
		conn.SetDeadline(deadline)
	} else {
		conn.SetDeadline(time.Now().Add(30 * time.Second))
	}
	if err := req.Write(conn); err != nil {
		return nil, fmt.Errorf("write request: %w", err)
	}
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, req)
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}
	if resp.StatusCode != http.StatusSwitchingProtocols && resp.StatusCode != http.StatusOK {
		data, _ := readLimited(resp.Body, maxResponse)
		return nil, parseAPIError(resp.StatusCode, data)
	}
	// The stream lives as long as the session, not the request.
	conn.SetDeadline(time.Time{})
	return &hijackedConn{Conn: conn, r: br}, nil
}
