package terminal

import (
	"bytes"
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/JCombee/wsm/internal/podman"
)

func TestParseControl(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		wantErr string
	}{
		{"valid", `{"type":"resize","rows":37,"cols":123}`, ""},
		{"unknown type", `{"type":"exec","rows":1,"cols":1}`, "unknown control type"},
		{"unknown field", `{"type":"resize","rows":1,"cols":1,"cmd":"sh"}`, "unknown field"},
		{"zero size", `{"type":"resize","rows":0,"cols":80}`, "out of range"},
		{"huge size", `{"type":"resize","rows":24,"cols":100000}`, "out of range"},
		{"trailing", `{"type":"resize","rows":1,"cols":1} {}`, "trailing data"},
		{"too long", `{"type":"resize","rows":1,"cols":1,"x":"` + strings.Repeat("a", 300) + `"}`, "exceeds"},
		{"not json", `resize`, "decode"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := ParseControl([]byte(tt.in))
			if tt.wantErr == "" {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("err = %v, want %q", err, tt.wantErr)
			}
		})
	}
}

// fakeExec is an exec whose "process" echoes its input in upper case.
type fakeExec struct {
	mu      sync.Mutex
	cfg     podman.ExecConfig
	start   [2]int
	resizes [][2]int
}

func (f *fakeExec) ExecCreate(_ context.Context, _ string, cfg podman.ExecConfig) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cfg = cfg
	return "exec1", nil
}

func (f *fakeExec) ExecStart(_ context.Context, _ string, _ bool, rows, cols int) (io.ReadWriteCloser, error) {
	f.mu.Lock()
	f.start = [2]int{rows, cols}
	f.mu.Unlock()
	client, proc := net.Pipe()
	go func() {
		defer proc.Close()
		buf := make([]byte, 1024)
		for {
			n, err := proc.Read(buf)
			if err != nil {
				return
			}
			if string(buf[:n]) == "exit\r" {
				return
			}
			proc.Write(bytes.ToUpper(buf[:n]))
		}
	}()
	return client, nil
}

func (f *fakeExec) ExecResize(_ context.Context, _ string, rows, cols int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.resizes = append(f.resizes, [2]int{rows, cols})
	return nil
}

// syncBuffer is a bytes.Buffer safe for concurrent use.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func TestServeAndAttach(t *testing.T) {
	fe := &fakeExec{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer ws.CloseNow()
		Serve(r.Context(), ws, fe, Session{Container: "c", Cmd: ShellCmd, WorkingDir: "/workspace", Rows: 30, Cols: 100})
	}))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	ws, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(srv.URL, "http"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer ws.CloseNow()

	stdinR, stdinW := io.Pipe()
	out := &syncBuffer{}
	resize := make(chan [2]int, 2)
	done := make(chan error, 1)
	go func() { done <- Attach(ctx, ws, stdinR, out, resize) }()

	resize <- [2]int{0, 0} // invalid sizes are not sent
	resize <- [2]int{40, 120}
	stdinW.Write([]byte("hello"))
	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(out.String(), "HELLO") && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if !strings.Contains(out.String(), "HELLO") {
		t.Fatalf("output = %q", out.String())
	}
	stdinW.Write([]byte("exit\r"))
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Attach: %v", err)
		}
	case <-ctx.Done():
		t.Fatal("Attach did not end after the session ended")
	}

	fe.mu.Lock()
	defer fe.mu.Unlock()
	if strings.Join(fe.cfg.Cmd, " ") != "tmux new-session -A -s main" || !fe.cfg.Tty || fe.cfg.WorkingDir != "/workspace" {
		t.Errorf("exec config %+v", fe.cfg)
	}
	if fe.start != [2]int{30, 100} {
		t.Errorf("start size %v", fe.start)
	}
	if len(fe.resizes) != 1 || fe.resizes[0] != [2]int{40, 120} {
		t.Errorf("resizes %v", fe.resizes)
	}
}

func TestInvalidControlClosesSession(t *testing.T) {
	fe := &fakeExec{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer ws.CloseNow()
		Serve(r.Context(), ws, fe, Session{Container: "c", Cmd: ShellCmd})
	}))
	defer srv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	ws, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(srv.URL, "http"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer ws.CloseNow()
	ws.Write(ctx, websocket.MessageText, []byte(`{"type":"exec","cmd":"sh"}`))
	for {
		_, _, err := ws.Read(ctx)
		if err != nil {
			if s := websocket.CloseStatus(err); s != websocket.StatusPolicyViolation {
				t.Fatalf("close status %v (%v), want policy violation", s, err)
			}
			return
		}
	}
}
