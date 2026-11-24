package main

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// wrapperScript runs the command under timeout, which puts itself and the
// command in a new process group. It records that group's PID, so stop and
// cancel can signal every process of the exec, and removes the file when the
// command ends. Stdin goes through fd 3 because an asynchronous command in
// a non-interactive shell would otherwise read /dev/null.
const wrapperScript = `f=$1
t=$2
shift 2
exec 3<&0
timeout -k 10s "${t}s" "$@" <&3 3<&- &
p=$!
exec 3<&-
printf '%s\n' "$p" >"$f" || { kill -KILL "$p" 2>/dev/null; exit 125; }
wait "$p"
c=$?
rm -f "$f"
exit "$c"`

// stopScript sends TERM to the exec's process group, then KILL after the
// grace period. It prints none, gone, term or kill. The PID file is written
// by the sandbox, which can only point it at its own processes.
//
// The group goes as "-PGID" right after the signal and never after "--":
// dash's kill rejects "--", and the fallback then reached only timeout.
const stopScript = `f=$1
g=$2
i=0
while [ ! -s "$f" ] && [ "$i" -lt 30 ]; do sleep 0.1; i=$((i + 1)); done
p=$(cat "$f" 2>/dev/null)
case $p in ''|*[!0-9]*) echo none; exit 0;; esac
grp() { kill "-$1" "-$p" 2>/dev/null; }
grp TERM || kill -TERM "$p" 2>/dev/null || { echo gone; exit 0; }
i=0
while grp 0; do
	if [ "$i" -ge $((g * 10)) ]; then grp KILL; echo kill; exit 0; fi
	sleep 0.1
	i=$((i + 1))
done
echo term`

const (
	stopGraceSeconds = 5
	// guardSlack is how long after its own deadline the server kills an
	// exec whose in-container timeout did not end it.
	guardSlack = 20 * time.Second
	// killWait is how long a stream may stay open after a kill.
	killWait = 15 * time.Second
)

// NewServer returns a server that runs execs over the projects' Podman
// sockets.
func NewServer(cfg Config, aud *Audit, admin Admin, logger *log.Logger) *Server {
	s := &Server{cfg: cfg, audit: aud, admin: admin, log: logger, podman: newPodmanExecer}
	s.ready = s.readyCheck
	return s
}

// target returns the project's Podman socket and the sandbox's work
// container, from values the management API returned.
func (s *Server) target(p Project, sb Sandbox) (string, string, error) {
	if !linuxUserRE.MatchString(p.LinuxUser) {
		return "", "", errors.New("unexpected linux user")
	}
	if !podNameRE.MatchString(sb.Pod) {
		return "", "", errors.New("unexpected pod name")
	}
	return filepath.Join(s.cfg.PodmanSocketDir, p.LinuxUser, "podman.sock"), sb.Pod + "-work", nil
}

// readyCheck waits until "true" exits 0 in the new sandbox.
func (s *Server) readyCheck(ctx context.Context, p Project, sb Sandbox) error {
	socket, container, err := s.target(p, sb)
	if err != nil {
		return err
	}
	ex := s.podman(socket)
	defer ex.Close()
	var last error
	for {
		actx, cancel := context.WithTimeout(ctx, 10*time.Second)
		code, _, err := ex.Run(actx, container, []string{"true"}, 64)
		cancel()
		if err == nil && code == 0 {
			return nil
		}
		last = err
		if last == nil {
			last = fmt.Errorf("exit %d", code)
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("%w (last: %v)", ctx.Err(), last)
		case <-time.After(time.Second):
		}
	}
}

func headerHasToken(h http.Header, name, token string) bool {
	for _, v := range h.Values(name) {
		for _, t := range strings.Split(v, ",") {
			if strings.EqualFold(strings.TrimSpace(t), token) {
				return true
			}
		}
	}
	return false
}

func randomHex(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// exec checks an exec request, audits it and then serves the stream on the
// hijacked connection. Every refusal comes before the upgrade, as a plain
// HTTP error.
func (h *tenantHandler) exec(rc *reqCtx, id string) {
	r := rc.r
	if !headerHasToken(r.Header, "Connection", "upgrade") || !strings.EqualFold(r.Header.Get("Upgrade"), execProtocol) {
		rc.deny(http.StatusBadRequest, "exec needs Connection: Upgrade and Upgrade: "+execProtocol)
		return
	}
	var spec ExecSpec
	if err := decodeStrict(r.Body, maxSpecBody, &spec); err != nil {
		rc.deny(http.StatusBadRequest, "invalid exec spec: "+err.Error())
		return
	}
	if err := spec.Validate(); err != nil {
		rc.deny(http.StatusBadRequest, "invalid exec spec: "+err.Error())
		return
	}
	rc.rec.Exec = spec.auditInfo()
	ctx := r.Context()
	sb, ok, err := h.lookup(ctx, id)
	switch {
	case err != nil:
		rc.fail(http.StatusBadGateway, "management api unavailable")
		return
	case !ok:
		rc.deny(http.StatusNotFound, noSuchSandbox)
		return
	case sb.Status != "running":
		rc.deny(http.StatusConflict, "sandbox is not running")
		return
	}
	p, err := h.s.admin.Project(ctx, h.t.Project)
	if err != nil {
		rc.fail(http.StatusBadGateway, "management api unavailable")
		return
	}
	socket, container, err := h.s.target(p, sb)
	if err != nil {
		rc.fail(http.StatusInternalServerError, err.Error())
		return
	}
	pidHex, err := randomHex(8)
	if err != nil {
		rc.fail(http.StatusInternalServerError, "no randomness")
		return
	}
	if n := h.execs.Add(1); int(n) > h.t.MaxExecs {
		h.execs.Add(-1)
		rc.deny(http.StatusTooManyRequests, "too many concurrent execs")
		return
	}
	defer h.execs.Add(-1)
	hj, ok := rc.w.(http.Hijacker)
	if !ok {
		rc.fail(http.StatusInternalServerError, "connection cannot be upgraded")
		return
	}
	if !rc.allow() {
		return
	}
	conn, brw, err := hj.Hijack()
	if err != nil {
		h.s.log.Printf("project=%s sandbox=%s hijack: %v", h.t.Project, id, err)
		return
	}
	conn.SetDeadline(time.Time{})
	var res sessionResult
	if _, err := brw.WriteString("HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: " +
		execProtocol + "\r\n\r\n"); err != nil || brw.Flush() != nil {
		conn.Close()
		res.Reason = "error"
	} else {
		ex := h.s.podman(socket)
		ss := &session{
			log: h.s.log, project: h.t.Project, sandbox: id,
			conn: conn, br: brw.Reader, fw: &frameWriter{w: conn},
			ex: ex, container: container, spec: spec,
			pidFile: "/tmp/.s06-exec-" + pidHex + ".pid", killDone: make(chan struct{}),
		}
		res = ss.run()
		ex.Close()
	}

	// The end line: how the exec ended, still without argv[1:] or values.
	end := rc.rec
	ea := *rc.rec.Exec
	ea.ExitCode, ea.TimedOut, ea.Stopped = res.ExitCode, res.TimedOut, res.Stopped
	ea.DurationMS = res.Duration.Milliseconds()
	ea.BytesIn, ea.BytesOut, ea.BytesErr = res.BytesIn, res.BytesOut, res.BytesErr
	end.Exec, end.Decision, end.Reason, end.Status, end.Time = &ea, "end", res.Reason, 0, ""
	if err := h.s.audit.Write(end); err != nil {
		h.s.log.Printf("project=%s sandbox=%s exec end: %v", h.t.Project, id, err)
	}
}

type sessionResult struct {
	ExitCode                    *int
	TimedOut, Stopped           bool
	Reason                      string
	Duration                    time.Duration
	BytesIn, BytesOut, BytesErr int64
}

// session is one exec stream between a client and a sandbox process.
type session struct {
	log              *log.Logger
	project, sandbox string
	conn             net.Conn
	br               *bufio.Reader
	fw               *frameWriter
	ex               Execer
	container        string
	spec             ExecSpec
	pidFile          string

	killOnce sync.Once
	killed   atomic.Bool
	killDone chan struct{}
	mu       sync.Mutex
	reason   string
	done     atomic.Bool

	bytesIn, bytesOut, bytesErr atomic.Int64
}

func (ss *session) logf(format string, args ...any) {
	ss.log.Printf("project=%s sandbox=%s "+format, append([]any{ss.project, ss.sandbox}, args...)...)
}

func (ss *session) getReason() string {
	ss.mu.Lock()
	defer ss.mu.Unlock()
	return ss.reason
}

// kill stops the process group in the sandbox through a second exec. The
// first cause wins.
func (ss *session) kill(reason string) {
	ss.killOnce.Do(func() {
		ss.mu.Lock()
		ss.reason = reason
		ss.mu.Unlock()
		ss.killed.Store(true)
		go func() {
			defer close(ss.killDone)
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			cmd := []string{"sh", "-c", stopScript, "s06-stop", ss.pidFile, strconv.Itoa(stopGraceSeconds)}
			code, out, err := ss.ex.Run(ctx, ss.container, cmd, 64)
			ss.logf("stop reason=%s result=%s code=%d err=%v", reason, strings.TrimSpace(string(out)), code, err)
		}()
	})
}

// closeLater closes the Podman stream if the process outlives a kill.
func (ss *session) closeLater(stream ExecStream) {
	time.AfterFunc(killWait, func() { stream.Close() })
}

// pumpIn copies the client's frames to the process.
func (ss *session) pumpIn(stream ExecStream) {
	eof := false
	for {
		typ, payload, err := readFrame(ss.br)
		if err != nil {
			if !ss.done.Load() {
				ss.kill("cancel")
				ss.closeLater(stream)
			}
			return
		}
		switch typ {
		case frameStdin:
			if !ss.spec.Stdin || eof {
				ss.protocolError(stream, "stdin data without stdin or after its end")
				return
			}
			if _, err := stream.Write(payload); err == nil {
				ss.bytesIn.Add(int64(len(payload)))
			}
		case frameStdinEOF:
			if !eof {
				eof = true
				_ = stream.CloseWrite()
			}
		case frameStop:
			ss.kill("stop")
			ss.closeLater(stream)
		default:
			ss.protocolError(stream, "unknown frame type")
			return
		}
	}
}

func (ss *session) protocolError(stream ExecStream, msg string) {
	_ = ss.fw.write(frameError, []byte(msg))
	ss.kill("protocol")
	ss.closeLater(stream)
}

// run starts the process and serves the stream until the process ends.
func (ss *session) run() sessionResult {
	start := time.Now()
	cfg := ExecConfig{
		AttachStdin: ss.spec.Stdin, AttachStdout: true, AttachStderr: true,
		Cmd: append([]string{"sh", "-c", wrapperScript, "s06-exec", ss.pidFile,
			strconv.Itoa(ss.spec.TimeoutSeconds)}, ss.spec.Argv...),
		Env: ss.spec.envList(), WorkingDir: ss.spec.Workdir,
	}
	sctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	stream, err := ss.ex.Start(sctx, ss.container, cfg)
	cancel()
	if err != nil {
		ss.logf("exec start: %v", err)
		_ = ss.fw.write(frameError, []byte("exec failed to start"))
		_ = ss.conn.Close()
		return sessionResult{Reason: "error", Duration: time.Since(start)}
	}
	defer stream.Close()
	guard := time.AfterFunc(time.Duration(ss.spec.TimeoutSeconds)*time.Second+guardSlack, func() {
		ss.kill("guard")
		ss.closeLater(stream)
	})
	defer guard.Stop()
	go ss.pumpIn(stream)

	clientGone, overLimit := false, false
	derr := demux(stream, func(which byte, p []byte) error {
		typ, counter, limit, name := frameStdout, &ss.bytesOut, ss.spec.StdoutLimit, "stdout"
		if which == 2 {
			typ, counter, limit, name = frameStderr, &ss.bytesErr, ss.spec.StderrLimit, "stderr"
		}
		if counter.Add(int64(len(p))) > limit && !overLimit {
			overLimit = true
			_ = ss.fw.write(frameError, fmt.Appendf(nil, "%s exceeds the limit of %d bytes", name, limit))
			ss.kill("limit")
			ss.closeLater(stream)
		}
		// After a limit or a lost client, drain until the process is gone.
		if overLimit || clientGone {
			return nil
		}
		if err := ss.fw.write(typ, p); err != nil {
			clientGone = true
			ss.kill("cancel")
			ss.closeLater(stream)
		}
		return nil
	})
	ss.done.Store(true)
	wctx, wcancel := context.WithTimeout(context.Background(), 10*time.Second)
	code, werr := stream.Wait(wctx)
	wcancel()

	res := sessionResult{Duration: time.Since(start), BytesIn: ss.bytesIn.Load(),
		BytesOut: ss.bytesOut.Load(), BytesErr: ss.bytesErr.Load()}
	if werr == nil {
		res.ExitCode = &code
	}
	res.Reason = ss.getReason()
	res.Stopped = res.Reason != ""
	res.TimedOut = werr == nil && code == 124 && res.Duration >= time.Duration(ss.spec.TimeoutSeconds)*time.Second
	if res.Reason == "" {
		switch {
		case res.TimedOut:
			res.Reason = "timeout"
		case derr != nil || werr != nil:
			res.Reason = "error"
			ss.logf("exec stream: demux=%v wait=%v", derr, werr)
		default:
			res.Reason = "exit"
		}
	}
	info, _ := json.Marshal(ExitInfo{Code: res.ExitCode, TimedOut: res.TimedOut, Stopped: res.Stopped, Reason: res.Reason})
	_ = ss.fw.write(frameExit, info)
	_ = ss.conn.Close()
	if ss.killed.Load() {
		select {
		case <-ss.killDone:
		case <-time.After(35 * time.Second):
		}
	}
	return res
}
