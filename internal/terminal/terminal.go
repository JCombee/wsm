// Package terminal connects a WebSocket to a Podman exec session with a TTY
// (plan section 3.8).
//
// Protocol over the WebSocket:
//   - binary messages carry raw terminal bytes, both ways;
//   - text messages from the client are control messages, for now only
//     {"type":"resize","rows":R,"cols":C}.
package terminal

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"github.com/coder/websocket"

	"github.com/JCombee/wsm/internal/podman"
)

// Limits on what the client sends.
const (
	maxControlMessage = 256
	MaxTermSize       = 1000
	// MaxInbound caps one WebSocket message from the client.
	MaxInbound = 64 << 10
)

// ShellCmd is the command a terminal session runs: one tmux session per
// sandbox, which survives disconnects and manager restarts.
var ShellCmd = []string{"tmux", "new-session", "-A", "-s", "main"}

// Control is a control message from the client.
type Control struct {
	Type string `json:"type"`
	Rows int    `json:"rows"`
	Cols int    `json:"cols"`
}

// ResizeMessage returns the control message for a new terminal size.
func ResizeMessage(rows, cols int) []byte {
	b, _ := json.Marshal(Control{Type: "resize", Rows: rows, Cols: cols})
	return b
}

// ValidSize reports whether rows and cols are a usable terminal size.
func ValidSize(rows, cols int) bool {
	return rows >= 1 && rows <= MaxTermSize && cols >= 1 && cols <= MaxTermSize
}

// ParseControl strictly parses a control message.
func ParseControl(b []byte) (Control, error) {
	var c Control
	if len(b) > maxControlMessage {
		return c, fmt.Errorf("control message exceeds %d bytes", maxControlMessage)
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&c); err != nil {
		return c, fmt.Errorf("decode control message: %w", err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return c, errors.New("trailing data after control message")
	}
	if c.Type != "resize" {
		return c, fmt.Errorf("unknown control type %q", c.Type)
	}
	if !ValidSize(c.Rows, c.Cols) {
		return c, fmt.Errorf("terminal size %dx%d out of range", c.Rows, c.Cols)
	}
	return c, nil
}

// Exec is the part of the Podman client a terminal session needs.
type Exec interface {
	ExecCreate(ctx context.Context, container string, cfg podman.ExecConfig) (string, error)
	ExecStart(ctx context.Context, id string, tty bool, rows, cols int) (io.ReadWriteCloser, error)
	ExecResize(ctx context.Context, id string, rows, cols int) error
}

// Session describes one terminal session.
type Session struct {
	Container  string
	Cmd        []string
	WorkingDir string
	Rows, Cols int
}

// Serve runs s in a new exec session and connects it to ws until either
// side ends. Output from the exec goes to the client as is: it is raw
// terminal data and is never interpreted by the manager.
func Serve(ctx context.Context, ws *websocket.Conn, pc Exec, s Session) error {
	ws.SetReadLimit(MaxInbound)
	if !ValidSize(s.Rows, s.Cols) {
		s.Rows, s.Cols = 24, 80
	}
	id, err := pc.ExecCreate(ctx, s.Container, podman.ExecConfig{
		AttachStdin:  true,
		AttachStdout: true,
		AttachStderr: true,
		Tty:          true,
		Cmd:          s.Cmd,
		Env:          []string{"TERM=xterm-256color"},
		WorkingDir:   s.WorkingDir,
	})
	if err != nil {
		return err
	}
	conn, err := pc.ExecStart(ctx, id, true, s.Rows, s.Cols)
	if err != nil {
		return err
	}
	defer conn.Close()

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		defer cancel()
		pump(ctx, conn, ws)
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
			c, err := ParseControl(data)
			if err != nil {
				ws.Close(websocket.StatusPolicyViolation, "invalid control message")
				return nil
			}
			if err := pc.ExecResize(ctx, id, c.Rows, c.Cols); err != nil {
				return err
			}
		}
	}
}

// pump copies exec output to the WebSocket until either side ends.
func pump(ctx context.Context, r io.Reader, ws *websocket.Conn) {
	buf := make([]byte, 32<<10)
	for {
		n, err := r.Read(buf)
		if n > 0 {
			if werr := ws.Write(ctx, websocket.MessageBinary, buf[:n]); werr != nil {
				return
			}
		}
		if err != nil {
			ws.Close(websocket.StatusNormalClosure, "session ended")
			return
		}
	}
}

// Attach is the client side: it copies stdin to the session, session
// output to stdout, and sends a resize message for every size on resize.
// It returns when the session or ctx ends.
func Attach(ctx context.Context, ws *websocket.Conn, stdin io.Reader, stdout io.Writer, resize <-chan [2]int) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	errc := make(chan error, 1)
	go func() {
		buf := make([]byte, 32<<10)
		for {
			n, err := stdin.Read(buf)
			if n > 0 {
				if werr := ws.Write(ctx, websocket.MessageBinary, buf[:n]); werr != nil {
					return
				}
			}
			if err != nil {
				return
			}
		}
	}()
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case sz, ok := <-resize:
				if !ok {
					return
				}
				if ValidSize(sz[0], sz[1]) {
					if err := ws.Write(ctx, websocket.MessageText, ResizeMessage(sz[0], sz[1])); err != nil {
						return
					}
				}
			}
		}
	}()
	go func() {
		for {
			typ, data, err := ws.Read(ctx)
			if err != nil {
				if s := websocket.CloseStatus(err); s == websocket.StatusNormalClosure || s == websocket.StatusGoingAway {
					errc <- nil
				} else {
					errc <- err
				}
				return
			}
			if typ != websocket.MessageBinary {
				continue
			}
			if _, err := stdout.Write(data); err != nil {
				errc <- err
				return
			}
		}
	}()
	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}
