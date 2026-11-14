package podman

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"time"
)

// MaxExecOutput caps the output collected from a non-TTY exec. Exec output
// comes from the sandbox and is untrusted.
const MaxExecOutput = 1 << 20

// demux splits Podman's multiplexed exec stream. Each frame has an 8-byte
// header: the stream (1 stdout, 2 stderr), three zero bytes and a big-endian
// uint32 payload size. It fails once more than limit payload bytes arrive.
func demux(r io.Reader, stdout, stderr io.Writer, limit int64) error {
	var hdr [8]byte
	var total int64
	for {
		if _, err := io.ReadFull(r, hdr[:]); err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return fmt.Errorf("read frame header: %w", err)
		}
		if hdr[1] != 0 || hdr[2] != 0 || hdr[3] != 0 {
			return fmt.Errorf("malformed frame header %x", hdr)
		}
		var w io.Writer
		switch hdr[0] {
		case 1:
			w = stdout
		case 2:
			w = stderr
		default:
			return fmt.Errorf("unexpected stream %d", hdr[0])
		}
		size := int64(binary.BigEndian.Uint32(hdr[4:]))
		total += size
		if total > limit {
			return fmt.Errorf("exec output exceeds %d bytes", limit)
		}
		if _, err := io.CopyN(w, r, size); err != nil {
			return fmt.Errorf("read frame payload: %w", err)
		}
	}
}

// limitedBuffer is an io.Writer that keeps at most max bytes.
type limitedBuffer struct {
	buf []byte
	max int
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	if len(b.buf)+len(p) > b.max {
		return 0, fmt.Errorf("output exceeds %d bytes", b.max)
	}
	b.buf = append(b.buf, p...)
	return len(p), nil
}

func (b *limitedBuffer) String() string { return string(b.buf) }

// ExecResult is the outcome of a non-TTY exec.
type ExecResult struct {
	Stdout   string
	Stderr   string
	ExitCode int
}

// Run executes cmd in container without a TTY and waits for it to exit.
func (c *Client) Run(ctx context.Context, container string, cmd ...string) (ExecResult, error) {
	return c.RunInput(ctx, container, nil, cmd...)
}

// halfCloser is a connection that can signal the end of its input.
type halfCloser interface {
	CloseWrite() error
}

// RunInput executes cmd in container without a TTY, with stdin as its
// standard input when stdin is not nil, and waits for it to exit. After
// the last byte of stdin the write side of the connection is closed, which
// Podman passes on to the process as EOF.
func (c *Client) RunInput(ctx context.Context, container string, stdin io.Reader, cmd ...string) (ExecResult, error) {
	var res ExecResult
	cfg := ExecConfig{AttachStdin: stdin != nil, AttachStdout: true, AttachStderr: true, Cmd: cmd}
	id, err := c.ExecCreate(ctx, container, cfg)
	if err != nil {
		return res, fmt.Errorf("exec %q: %w", cmd, err)
	}
	conn, err := c.ExecStart(ctx, id, false, 0, 0)
	if err != nil {
		return res, fmt.Errorf("exec %q: %w", cmd, err)
	}
	// Closing the connection ends the stdin copy and the demux.
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	defer stop()
	inErr := make(chan error, 1)
	if stdin != nil {
		hc, ok := conn.(halfCloser)
		if !ok {
			conn.Close()
			return res, fmt.Errorf("exec %q: the connection cannot signal the end of stdin", cmd)
		}
		go func() {
			_, err := io.Copy(conn, stdin)
			if cerr := hc.CloseWrite(); err == nil {
				err = cerr
			}
			inErr <- err
		}()
	} else {
		inErr <- nil
	}
	stdout := &limitedBuffer{max: MaxExecOutput}
	stderr := &limitedBuffer{max: MaxExecOutput}
	err = demux(conn, stdout, stderr, MaxExecOutput)
	conn.Close()
	ierr := <-inErr
	res.Stdout, res.Stderr = stdout.String(), stderr.String()
	if ctx.Err() != nil {
		return res, fmt.Errorf("exec %q: %w", cmd, ctx.Err())
	}
	if err != nil {
		return res, fmt.Errorf("exec %q: %w", cmd, err)
	}
	if ierr != nil {
		return res, fmt.Errorf("exec %q: stdin: %w", cmd, ierr)
	}
	// The stream can end a moment before Podman records the exit code.
	for range 50 {
		st, err := c.ExecInspect(ctx, id)
		if err != nil {
			return res, fmt.Errorf("exec %q: %w", cmd, err)
		}
		if !st.Running {
			res.ExitCode = st.ExitCode
			return res, nil
		}
		select {
		case <-ctx.Done():
			return res, ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
	return res, fmt.Errorf("exec %q: still running after its stream closed", cmd)
}

// Output runs cmd and returns stdout, failing on a non-zero exit code.
func (c *Client) Output(ctx context.Context, container string, cmd ...string) (string, error) {
	res, err := c.Run(ctx, container, cmd...)
	if err != nil {
		return "", err
	}
	if res.ExitCode != 0 {
		return "", fmt.Errorf("exec %q: exit code %d: %q", cmd, res.ExitCode, res.Stderr)
	}
	return res.Stdout, nil
}
