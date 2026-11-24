package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"io"
	"log"
	"net"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeStream plays a Podman exec session: the test writes multiplexed
// output and ends the process; the session writes stdin.
type fakeStream struct {
	out  *io.PipeReader
	outW *io.PipeWriter

	mu          sync.Mutex
	stdin       bytes.Buffer
	closedWrite bool
	code        int
}

func newFakeStream() *fakeStream {
	r, w := io.Pipe()
	return &fakeStream{out: r, outW: w}
}

func (s *fakeStream) Read(p []byte) (int, error) { return s.out.Read(p) }
func (s *fakeStream) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.stdin.Write(p)
}
func (s *fakeStream) CloseWrite() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closedWrite = true
	return nil
}
func (s *fakeStream) Close() error { return s.out.Close() }
func (s *fakeStream) Wait(context.Context) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.code, nil
}

func (s *fakeStream) emit(stream byte, data string) {
	var hdr [8]byte
	hdr[0] = stream
	binary.BigEndian.PutUint32(hdr[4:], uint32(len(data)))
	_, _ = s.outW.Write(append(hdr[:], data...))
}

func (s *fakeStream) end(code int) {
	s.mu.Lock()
	s.code = code
	s.mu.Unlock()
	_ = s.outW.Close()
}

type fakeExecer struct {
	stream *fakeStream
	mu     sync.Mutex
	cfg    ExecConfig
	runs   [][]string
	onStop func()
}

func (f *fakeExecer) Start(_ context.Context, _ string, cfg ExecConfig) (ExecStream, error) {
	f.mu.Lock()
	f.cfg = cfg
	f.mu.Unlock()
	return f.stream, nil
}

func (f *fakeExecer) Run(_ context.Context, _ string, cmd []string, _ int64) (int, []byte, error) {
	f.mu.Lock()
	f.runs = append(f.runs, cmd)
	stop := f.onStop
	f.mu.Unlock()
	if stop != nil {
		stop()
	}
	return 0, []byte("term\n"), nil
}

func (f *fakeExecer) Close() {}

func (f *fakeExecer) stopRuns() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.runs {
		if len(c) > 3 && c[3] == "s06-stop" {
			n++
		}
	}
	return n
}

type frame struct {
	typ     byte
	payload []byte
}

// startSession runs a session against fake and returns the client end, the
// frames the client receives, and the session result.
func startSession(t *testing.T, fake *fakeExecer, spec ExecSpec) (net.Conn, <-chan frame, <-chan sessionResult) {
	t.Helper()
	if err := spec.Validate(); err != nil {
		t.Fatal(err)
	}
	server, client := net.Pipe()
	ss := &session{
		log: log.New(io.Discard, "", 0), project: "s06a", sandbox: "t0123456789",
		conn: server, br: bufio.NewReader(server), fw: &frameWriter{w: server},
		ex: fake, container: "c", spec: spec, pidFile: "/tmp/.s06-exec-x.pid", killDone: make(chan struct{}),
	}
	frames := make(chan frame, 64)
	go func() {
		defer close(frames)
		for {
			typ, p, err := readFrame(client)
			if err != nil {
				return
			}
			frames <- frame{typ, p}
		}
	}()
	results := make(chan sessionResult, 1)
	go func() { results <- ss.run() }()
	return client, frames, results
}

func collect(t *testing.T, frames <-chan frame) (stdout, stderr, errs string, exit ExitInfo) {
	t.Helper()
	timeout := time.After(10 * time.Second)
	for {
		select {
		case f, ok := <-frames:
			if !ok {
				return
			}
			switch f.typ {
			case frameStdout:
				stdout += string(f.payload)
			case frameStderr:
				stderr += string(f.payload)
			case frameError:
				errs += string(f.payload)
			case frameExit:
				if err := json.Unmarshal(f.payload, &exit); err != nil {
					t.Fatal(err)
				}
			}
		case <-timeout:
			t.Fatal("no end of stream")
		}
	}
}

func spec(argv ...string) ExecSpec { return ExecSpec{Argv: argv, TimeoutSeconds: 30} }

func TestSessionExit(t *testing.T) {
	fs := newFakeStream()
	fake := &fakeExecer{stream: fs}
	_, frames, results := startSession(t, fake, ExecSpec{Argv: []string{"sh"}, Env: map[string]string{"K": "v\nw"}, TimeoutSeconds: 30})
	go func() {
		fs.emit(1, "out\n")
		fs.emit(2, "err\n")
		fs.end(7)
	}()
	stdout, stderr, errs, exit := collect(t, frames)
	res := <-results
	if stdout != "out\n" || stderr != "err\n" || errs != "" || exit.Code == nil || *exit.Code != 7 || exit.Reason != "exit" {
		t.Fatalf("stdout %q stderr %q errs %q exit %+v", stdout, stderr, errs, exit)
	}
	if res.Stopped || fake.stopRuns() != 0 {
		t.Fatalf("a normal exit stopped the process: %+v, %d stop runs", res, fake.stopRuns())
	}
	cmd := strings.Join(fake.cfg.Cmd, " ")
	if !strings.HasPrefix(cmd, "sh -c ") || !strings.HasSuffix(cmd, " 30 sh") || fake.cfg.Env[0] != "K=v\nw" {
		t.Fatalf("exec config: cmd %q env %q", cmd, fake.cfg.Env)
	}
}

func TestSessionOutputLimit(t *testing.T) {
	fs := newFakeStream()
	fake := &fakeExecer{stream: fs}
	fake.onStop = func() { fs.end(137) }
	s := spec("yes")
	s.StdoutLimit = 10
	_, frames, results := startSession(t, fake, s)
	go fs.emit(1, strings.Repeat("y\n", 10))
	_, _, errs, exit := collect(t, frames)
	res := <-results
	if !strings.Contains(errs, "limit of 10 bytes") || !exit.Stopped || exit.Reason != "limit" || fake.stopRuns() != 1 {
		t.Fatalf("errs %q exit %+v result %+v stop runs %d", errs, exit, res, fake.stopRuns())
	}
}

func TestSessionStopFrame(t *testing.T) {
	fs := newFakeStream()
	fake := &fakeExecer{stream: fs}
	fake.onStop = func() { fs.end(143) }
	client, frames, results := startSession(t, fake, spec("sleep", "300"))
	if err := writeFrame(client, frameStop, nil); err != nil {
		t.Fatal(err)
	}
	_, _, _, exit := collect(t, frames)
	res := <-results
	if !exit.Stopped || exit.Reason != "stop" || res.Reason != "stop" || fake.stopRuns() != 1 {
		t.Fatalf("exit %+v result %+v stop runs %d", exit, res, fake.stopRuns())
	}
}

func TestSessionCancelOnDisconnect(t *testing.T) {
	fs := newFakeStream()
	fake := &fakeExecer{stream: fs}
	fake.onStop = func() { fs.end(143) }
	client, _, results := startSession(t, fake, spec("sleep", "300"))
	time.Sleep(50 * time.Millisecond)
	client.Close()
	select {
	case res := <-results:
		if res.Reason != "cancel" || !res.Stopped || fake.stopRuns() != 1 {
			t.Fatalf("result %+v stop runs %d", res, fake.stopRuns())
		}
	case <-time.After(10 * time.Second):
		t.Fatal("session did not end after the client left")
	}
}

func TestSessionStdin(t *testing.T) {
	fs := newFakeStream()
	fake := &fakeExecer{stream: fs}
	s := spec("cat")
	s.Stdin = true
	client, frames, results := startSession(t, fake, s)
	if err := writeFrame(client, frameStdin, []byte("abc")); err != nil {
		t.Fatal(err)
	}
	if err := writeFrame(client, frameStdinEOF, nil); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		fs.mu.Lock()
		done := fs.closedWrite
		fs.mu.Unlock()
		if done || time.Now().After(deadline) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	fs.end(0)
	_, _, _, exit := collect(t, frames)
	res := <-results
	if fs.stdin.String() != "abc" || !fs.closedWrite || exit.Code == nil || *exit.Code != 0 || res.BytesIn != 3 {
		t.Fatalf("stdin %q closed %v exit %+v result %+v", fs.stdin.String(), fs.closedWrite, exit, res)
	}
}

func TestSessionStdinWithoutStdinIsAProtocolError(t *testing.T) {
	fs := newFakeStream()
	fake := &fakeExecer{stream: fs}
	fake.onStop = func() { fs.end(143) }
	client, frames, results := startSession(t, fake, spec("true"))
	if err := writeFrame(client, frameStdin, []byte("x")); err != nil {
		t.Fatal(err)
	}
	_, _, errs, exit := collect(t, frames)
	<-results
	if !strings.Contains(errs, "stdin") || exit.Reason != "protocol" {
		t.Fatalf("errs %q exit %+v", errs, exit)
	}
}
