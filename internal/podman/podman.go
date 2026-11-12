// Package podman is a thin client for the libpod REST API on a project
// user's Podman socket (ADR 0003).
//
// The socket belongs to a process the project user controls, so every
// response is untrusted input (CLAUDE.md invariant 8): bodies are size
// limited and decoded into typed structs only.
//
// The container spec has no field for bind mounts or devices, and none for
// privileged mode or host namespaces: the client cannot express them.
package podman

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

// MaxResponse caps every JSON response body read from Podman.
const MaxResponse = 1 << 20

// DialFunc opens a connection to the Podman API.
type DialFunc func(ctx context.Context) (net.Conn, error)

// Client is a libpod REST client over one Podman API socket.
type Client struct {
	dial DialFunc
	http *http.Client
}

// NewClient returns a client for the Podman API socket at path.
func NewClient(path string) *Client {
	return NewClientDialer(func(ctx context.Context) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "unix", path)
	})
}

// NewClientDialer returns a client that connects with dial.
func NewClientDialer(dial DialFunc) *Client {
	c := &Client{dial: dial}
	c.http = &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			conn, err := c.dial(ctx)
			if err != nil {
				return nil, fmt.Errorf("dial podman: %w", err)
			}
			return conn, nil
		},
		DisableCompression: true,
		MaxIdleConns:       2,
		IdleConnTimeout:    30 * time.Second,
		// The project user runs the server, so a stalled answer must not
		// hold the manager for long. Pod stop answers after its own stop
		// timeout; nothing else is slow, since wsm pulls no images.
		ResponseHeaderTimeout: 2 * time.Minute,
	}}
	return c
}

// Close releases idle connections.
func (c *Client) Close() { c.http.CloseIdleConnections() }

// APIError is an error response from Podman.
type APIError struct {
	Status  int
	Message string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("podman api: status %d: %s", e.Status, e.Message)
}

// IsNotFound reports whether err is a 404 from Podman.
func IsNotFound(err error) bool {
	var apiErr *APIError
	return errors.As(err, &apiErr) && apiErr.Status == http.StatusNotFound
}

// maxErrorMessage caps an error message taken from a response.
const maxErrorMessage = 512

func parseAPIError(status int, body []byte) error {
	var b struct {
		Message string `json:"message"`
	}
	apiErr := &APIError{Status: status}
	if err := json.Unmarshal(body, &b); err == nil && b.Message != "" {
		apiErr.Message = b.Message
	} else {
		apiErr.Message = http.StatusText(status)
	}
	if len(apiErr.Message) > maxErrorMessage {
		apiErr.Message = apiErr.Message[:maxErrorMessage] + "..."
	}
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

// decode parses exactly one JSON value into out.
func decode(data []byte, out any) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	if err := dec.Decode(out); err != nil {
		return err
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return errors.New("trailing data after the response")
	}
	return nil
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
	data, err := readLimited(resp.Body, MaxResponse)
	if err != nil {
		return fmt.Errorf("%s %s: %w", method, path, err)
	}
	if resp.StatusCode >= 300 && resp.StatusCode != http.StatusNotModified {
		return fmt.Errorf("%s %s: %w", method, path, parseAPIError(resp.StatusCode, data))
	}
	if out == nil {
		return nil
	}
	if err := decode(data, out); err != nil {
		return fmt.Errorf("%s %s: decode response: %w", method, path, err)
	}
	return nil
}

// Namespace is a libpod namespace setting, e.g. {"nsmode": "keep-id"}.
type Namespace struct {
	Mode  string `json:"nsmode"`
	Value string `json:"value,omitempty"`
}

// PodSpec is the subset of libpod's PodSpecGenerator wsm uses.
type PodSpec struct {
	Name     string            `json:"name"`
	Labels   map[string]string `json:"labels,omitempty"`
	Userns   *Namespace        `json:"userns,omitempty"`
	Netns    *Namespace        `json:"netns,omitempty"`
	Hostname string            `json:"hostname,omitempty"`
}

// Resources is the subset of the OCI LinuxResources wsm sets.
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

// NamedVolume mounts a Podman named volume. There is no type for bind
// mounts: host paths never go into a sandbox (CLAUDE.md invariant 1).
type NamedVolume struct {
	Name    string   `json:"Name"`
	Dest    string   `json:"Dest"`
	Options []string `json:"Options,omitempty"`
}

// ContainerSpec is the subset of libpod's SpecGenerator wsm uses.
type ContainerSpec struct {
	Name            string            `json:"name"`
	Image           string            `json:"image"`
	Pod             string            `json:"pod,omitempty"`
	Labels          map[string]string `json:"labels,omitempty"`
	User            string            `json:"user,omitempty"`
	Command         []string          `json:"command,omitempty"`
	Env             map[string]string `json:"env,omitempty"`
	WorkDir         string            `json:"work_dir,omitempty"`
	Init            bool              `json:"init,omitempty"`
	CapDrop         []string          `json:"cap_drop,omitempty"`
	NoNewPrivileges bool              `json:"no_new_privileges,omitempty"`
	Resources       *Resources        `json:"resource_limits,omitempty"`
	Volumes         []NamedVolume     `json:"volumes,omitempty"`
}

type idResponse struct {
	ID string `json:"Id"`
}

// PodCreate creates a pod and returns its ID.
func (c *Client) PodCreate(ctx context.Context, spec PodSpec) (string, error) {
	var r idResponse
	if err := c.do(ctx, http.MethodPost, "/pods/create", nil, spec, &r); err != nil {
		return "", err
	}
	return r.ID, nil
}

// podReport is what pod start and stop return.
type podReport struct {
	Errs []string `json:"Errs"`
	ID   string   `json:"Id"`
}

func (c *Client) podAction(ctx context.Context, name, action string, q url.Values) error {
	var r podReport
	err := c.do(ctx, http.MethodPost, "/pods/"+url.PathEscape(name)+"/"+action, q, nil, &r)
	if err != nil {
		return err
	}
	if len(r.Errs) > 0 {
		return fmt.Errorf("pod %s %s: %s", action, name, r.Errs[0])
	}
	return nil
}

// PodStart starts every container in a pod.
func (c *Client) PodStart(ctx context.Context, name string) error {
	return c.podAction(ctx, name, "start", nil)
}

// PodStop stops every container in a pod.
func (c *Client) PodStop(ctx context.Context, name string, timeout time.Duration) error {
	q := url.Values{"t": {strconv.Itoa(int(timeout.Seconds()))}}
	return c.podAction(ctx, name, "stop", q)
}

// PodRemove force-removes a pod and its containers. A missing pod is not an
// error.
func (c *Client) PodRemove(ctx context.Context, name string) error {
	q := url.Values{"force": {"true"}}
	err := c.do(ctx, http.MethodDelete, "/pods/"+url.PathEscape(name), q, nil, nil)
	if IsNotFound(err) {
		return nil
	}
	return err
}

// ContainerCreate creates a container and returns its ID.
func (c *Client) ContainerCreate(ctx context.Context, spec ContainerSpec) (string, error) {
	var r idResponse
	if err := c.do(ctx, http.MethodPost, "/containers/create", nil, spec, &r); err != nil {
		return "", err
	}
	return r.ID, nil
}

// VolumeCreate creates a named volume. An existing volume is not an error.
func (c *Client) VolumeCreate(ctx context.Context, name string, labels map[string]string) error {
	exists, err := c.VolumeExists(ctx, name)
	if err != nil || exists {
		return err
	}
	in := struct {
		Name   string            `json:"Name"`
		Labels map[string]string `json:"Labels,omitempty"`
	}{name, labels}
	var out struct {
		Name string `json:"Name"`
	}
	return c.do(ctx, http.MethodPost, "/volumes/create", nil, in, &out)
}

// VolumeExists reports whether a named volume exists.
func (c *Client) VolumeExists(ctx context.Context, name string) (bool, error) {
	err := c.do(ctx, http.MethodGet, "/volumes/"+url.PathEscape(name)+"/exists", nil, nil, nil)
	if IsNotFound(err) {
		return false, nil
	}
	return err == nil, err
}

// VolumeRemove removes a named volume. A missing volume is not an error.
func (c *Client) VolumeRemove(ctx context.Context, name string) error {
	q := url.Values{"force": {"true"}}
	err := c.do(ctx, http.MethodDelete, "/volumes/"+url.PathEscape(name), q, nil, nil)
	if IsNotFound(err) {
		return nil
	}
	return err
}

// ImageExists reports whether an image is present locally.
func (c *Client) ImageExists(ctx context.Context, ref string) (bool, error) {
	err := c.do(ctx, http.MethodGet, "/images/"+url.PathEscape(ref)+"/exists", nil, nil, nil)
	if IsNotFound(err) {
		return false, nil
	}
	return err == nil, err
}

// ImageConfig is the subset of an image's config wsm reads.
type ImageConfig struct {
	Cmd        []string `json:"Cmd"`
	Entrypoint []string `json:"Entrypoint"`
}

// ImageInspect returns the config of a local image.
func (c *Client) ImageInspect(ctx context.Context, ref string) (ImageConfig, error) {
	var r struct {
		Config ImageConfig `json:"Config"`
	}
	err := c.do(ctx, http.MethodGet, "/images/"+url.PathEscape(ref)+"/json", nil, nil, &r)
	return r.Config, err
}

// ExecConfig is the body of an exec create request.
type ExecConfig struct {
	AttachStdin  bool     `json:"AttachStdin"`
	AttachStdout bool     `json:"AttachStdout"`
	AttachStderr bool     `json:"AttachStderr"`
	Tty          bool     `json:"Tty"`
	Cmd          []string `json:"Cmd"`
	Env          []string `json:"Env,omitempty"`
	WorkingDir   string   `json:"WorkingDir,omitempty"`
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
// With a TTY the stream is raw; without one Podman multiplexes stdout and
// stderr into frames.
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
	conn, err := c.dial(ctx)
	if err != nil {
		return nil, fmt.Errorf("dial podman: %w", err)
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
		data, _ := readLimited(resp.Body, MaxResponse)
		return nil, parseAPIError(resp.StatusCode, data)
	}
	// The stream lives as long as the session, not the request.
	conn.SetDeadline(time.Time{})
	return &hijackedConn{Conn: conn, r: br}, nil
}
