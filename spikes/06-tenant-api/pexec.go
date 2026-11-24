package main

// A minimal libpod REST client for exec, taken from wsm's internal/podman
// (ADR 0003), which a separate module cannot import. Everything the Podman
// socket returns is untrusted: bodies are size limited.

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"time"
)

const (
	podmanAPI         = "v5.0.0"
	maxPodmanResponse = 1 << 20
	// execUser is the sandbox's non-root user (CLAUDE.md invariant 9).
	execUser = "1000:1000"
)

// ExecConfig is the body of a libpod exec create request.
type ExecConfig struct {
	AttachStdin  bool     `json:"AttachStdin"`
	AttachStdout bool     `json:"AttachStdout"`
	AttachStderr bool     `json:"AttachStderr"`
	Tty          bool     `json:"Tty"`
	Privileged   bool     `json:"Privileged"`
	User         string   `json:"User"`
	Cmd          []string `json:"Cmd"`
	Env          []string `json:"Env,omitempty"`
	WorkingDir   string   `json:"WorkingDir,omitempty"`
}

// ExecStream is a started exec session: reads give Podman's multiplexed
// stdout and stderr, writes go to stdin.
type ExecStream interface {
	io.ReadWriteCloser
	CloseWrite() error
	// Wait returns the exit code once Podman records it.
	Wait(ctx context.Context) (int, error)
}

// Execer runs processes in a sandbox's work container.
type Execer interface {
	Start(ctx context.Context, container string, cfg ExecConfig) (ExecStream, error)
	// Run runs a short command and returns its exit code and up to limit
	// bytes of stdout.
	Run(ctx context.Context, container string, cmd []string, limit int64) (int, []byte, error)
	Close()
}

type podmanClient struct {
	socket string
	http   *http.Client
}

func newPodmanExecer(socket string) Execer {
	c := &podmanClient{socket: socket}
	c.http = &http.Client{Transport: &http.Transport{
		DialContext:           func(ctx context.Context, _, _ string) (net.Conn, error) { return c.dial(ctx) },
		DisableCompression:    true,
		MaxIdleConns:          2,
		IdleConnTimeout:       30 * time.Second,
		ResponseHeaderTimeout: 2 * time.Minute,
	}}
	return c
}

func (c *podmanClient) dial(ctx context.Context) (net.Conn, error) {
	var d net.Dialer
	conn, err := d.DialContext(ctx, "unix", c.socket)
	if err != nil {
		return nil, fmt.Errorf("dial podman: %w", err)
	}
	return conn, nil
}

func (c *podmanClient) Close() { c.http.CloseIdleConnections() }

func podmanURL(path string) string { return "http://podman/" + podmanAPI + "/libpod" + path }

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

func podmanError(status int, body []byte) error {
	var b struct {
		Message string `json:"message"`
	}
	msg := http.StatusText(status)
	if json.Unmarshal(body, &b) == nil && b.Message != "" {
		msg = b.Message
	}
	if len(msg) > 512 {
		msg = msg[:512]
	}
	return fmt.Errorf("podman api: status %d: %s", status, msg)
}

func (c *podmanClient) do(ctx context.Context, method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, podmanURL(path), body)
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
	data, err := readLimited(resp.Body, maxPodmanResponse)
	if err != nil {
		return fmt.Errorf("%s %s: %w", method, path, err)
	}
	if resp.StatusCode >= 300 {
		return fmt.Errorf("%s %s: %w", method, path, podmanError(resp.StatusCode, data))
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(data, out)
}

type hijackedConn struct {
	net.Conn
	r *bufio.Reader
}

func (h *hijackedConn) Read(p []byte) (int, error) { return h.r.Read(p) }

func (h *hijackedConn) CloseWrite() error {
	if cw, ok := h.Conn.(interface{ CloseWrite() error }); ok {
		return cw.CloseWrite()
	}
	return errors.New("connection cannot half-close")
}

// hijack posts to path and returns the raw connection once Podman answers
// with 101 or 200, after which the exec stream starts.
func (c *podmanClient) hijack(ctx context.Context, path string, in any) (*hijackedConn, error) {
	b, err := json.Marshal(in)
	if err != nil {
		return nil, err
	}
	conn, err := c.dial(ctx)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, podmanURL(path), bytes.NewReader(b))
	if err != nil {
		conn.Close()
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", "tcp")
	if deadline, ok := ctx.Deadline(); ok {
		conn.SetDeadline(deadline)
	} else {
		conn.SetDeadline(time.Now().Add(30 * time.Second))
	}
	if err := req.Write(conn); err != nil {
		conn.Close()
		return nil, fmt.Errorf("POST %s: %w", path, err)
	}
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, req)
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("POST %s: %w", path, err)
	}
	if resp.StatusCode != http.StatusSwitchingProtocols && resp.StatusCode != http.StatusOK {
		data, _ := readLimited(resp.Body, maxPodmanResponse)
		conn.Close()
		return nil, fmt.Errorf("POST %s: %w", path, podmanError(resp.StatusCode, data))
	}
	conn.SetDeadline(time.Time{})
	return &hijackedConn{Conn: conn, r: br}, nil
}

type podmanStream struct {
	*hijackedConn
	c  *podmanClient
	id string
}

func (s *podmanStream) Wait(ctx context.Context) (int, error) {
	for {
		var st struct {
			Running  bool `json:"Running"`
			ExitCode int  `json:"ExitCode"`
		}
		if err := s.c.do(ctx, http.MethodGet, "/exec/"+url.PathEscape(s.id)+"/json", nil, &st); err != nil {
			return 0, err
		}
		if !st.Running {
			return st.ExitCode, nil
		}
		select {
		case <-ctx.Done():
			return 0, ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
	}
}

func (c *podmanClient) Start(ctx context.Context, container string, cfg ExecConfig) (ExecStream, error) {
	cfg.User, cfg.Privileged, cfg.Tty = execUser, false, false
	var r struct {
		ID string `json:"Id"`
	}
	if err := c.do(ctx, http.MethodPost, "/containers/"+url.PathEscape(container)+"/exec", cfg, &r); err != nil {
		return nil, err
	}
	conn, err := c.hijack(ctx, "/exec/"+url.PathEscape(r.ID)+"/start", map[string]bool{"Detach": false, "Tty": false})
	if err != nil {
		return nil, err
	}
	return &podmanStream{hijackedConn: conn, c: c, id: r.ID}, nil
}

func (c *podmanClient) Run(ctx context.Context, container string, cmd []string, limit int64) (int, []byte, error) {
	st, err := c.Start(ctx, container, ExecConfig{AttachStdout: true, AttachStderr: true, Cmd: cmd})
	if err != nil {
		return 0, nil, err
	}
	defer st.Close()
	stop := context.AfterFunc(ctx, func() { st.Close() })
	defer stop()
	var out bytes.Buffer
	if err := demux(st, func(stream byte, p []byte) error {
		if stream == 1 && int64(out.Len()+len(p)) <= limit {
			out.Write(p)
		}
		return nil
	}); err != nil {
		return 0, out.Bytes(), err
	}
	code, err := st.Wait(ctx)
	return code, out.Bytes(), err
}

// demux splits Podman's multiplexed exec stream: each frame has an 8-byte
// header with the stream (1 stdout, 2 stderr), three zero bytes and a
// big-endian uint32 size. fn gets the payload in chunks of at most 32 KiB.
func demux(r io.Reader, fn func(stream byte, p []byte) error) error {
	var hdr [8]byte
	buf := make([]byte, 32<<10)
	for {
		if _, err := io.ReadFull(r, hdr[:]); err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return fmt.Errorf("read frame header: %w", err)
		}
		if (hdr[0] != 1 && hdr[0] != 2) || hdr[1] != 0 || hdr[2] != 0 || hdr[3] != 0 {
			return fmt.Errorf("malformed frame header %x", hdr)
		}
		size := int64(binary.BigEndian.Uint32(hdr[4:]))
		for size > 0 {
			n := min(size, int64(len(buf)))
			if _, err := io.ReadFull(r, buf[:n]); err != nil {
				return fmt.Errorf("read frame payload: %w", err)
			}
			if err := fn(hdr[0], buf[:n]); err != nil {
				return err
			}
			size -= n
		}
	}
}
