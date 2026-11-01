package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"

	"github.com/coder/websocket"
)

// Terminal protocol over the WebSocket:
//   - binary messages carry raw terminal bytes, both ways;
//   - text messages from the client are control messages, for now only
//     {"type":"resize","rows":R,"cols":C}.

const (
	maxControlMessage = 256
	maxTermSize       = 1000
	maxInbound        = 64 << 10
)

type control struct {
	Type string `json:"type"`
	Rows int    `json:"rows"`
	Cols int    `json:"cols"`
}

// parseControl strictly parses a control message from the browser.
func parseControl(b []byte) (control, error) {
	var c control
	if len(b) > maxControlMessage {
		return c, fmt.Errorf("control message exceeds %d bytes", maxControlMessage)
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&c); err != nil {
		return c, fmt.Errorf("decode control message: %w", err)
	}
	if dec.More() {
		return c, errors.New("trailing data after control message")
	}
	if c.Type != "resize" {
		return c, fmt.Errorf("unknown control type %q", c.Type)
	}
	if c.Rows < 1 || c.Rows > maxTermSize || c.Cols < 1 || c.Cols > maxTermSize {
		return c, fmt.Errorf("terminal size %dx%d out of range", c.Rows, c.Cols)
	}
	return c, nil
}

// Bridge connects each WebSocket to a new exec session with a TTY.
type Bridge struct {
	Podman    *Client
	Container string
	Cmd       []string
	Logf      func(format string, args ...any)
}

func (b *Bridge) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	ws, err := websocket.Accept(w, r, nil)
	if err != nil {
		b.Logf("bridge: accept: %v", err)
		return
	}
	defer ws.CloseNow()
	ws.SetReadLimit(maxInbound)
	if err := b.serve(r.Context(), ws); err != nil {
		b.Logf("bridge: %v", err)
		ws.Close(websocket.StatusInternalError, "terminal error")
		return
	}
	ws.Close(websocket.StatusNormalClosure, "")
}

func (b *Bridge) serve(ctx context.Context, ws *websocket.Conn) error {
	id, err := b.Podman.ExecCreate(ctx, b.Container, ExecConfig{
		AttachStdin:  true,
		AttachStdout: true,
		AttachStderr: true,
		Tty:          true,
		Cmd:          b.Cmd,
		Env:          []string{"TERM=xterm-256color"},
	})
	if err != nil {
		return err
	}
	conn, err := b.Podman.ExecStart(ctx, id, true, 24, 80)
	if err != nil {
		return err
	}
	defer conn.Close()

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		defer cancel()
		pumpOutput(ctx, conn, ws)
	}()

	for {
		typ, data, err := ws.Read(ctx)
		if err != nil {
			if ctx.Err() != nil || websocket.CloseStatus(err) != -1 {
				return nil // the exec ended or the client closed
			}
			return fmt.Errorf("read websocket: %w", err)
		}
		switch typ {
		case websocket.MessageBinary:
			if _, err := conn.Write(data); err != nil {
				return fmt.Errorf("write to exec: %w", err)
			}
		case websocket.MessageText:
			c, err := parseControl(data)
			if err != nil {
				ws.Close(websocket.StatusPolicyViolation, "invalid control message")
				return nil
			}
			if err := b.Podman.ExecResize(ctx, id, c.Rows, c.Cols); err != nil {
				return err
			}
		}
	}
}

// pumpOutput copies exec output to the WebSocket until either side ends.
func pumpOutput(ctx context.Context, r io.Reader, ws *websocket.Conn) {
	buf := make([]byte, 32<<10)
	for {
		n, err := r.Read(buf)
		if n > 0 {
			if werr := ws.Write(ctx, websocket.MessageBinary, buf[:n]); werr != nil {
				return
			}
		}
		if err != nil {
			return
		}
	}
}
