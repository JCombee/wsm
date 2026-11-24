package main

import (
	"archive/tar"
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	mrand "math/rand/v2"
	"net"
	"net/http"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

// client plays the control room. It prints one JSON line per finding on
// stdout: results, info, and every request ID it sent, so the run can match
// them against the audit. It never prints a sentinel.
type client struct {
	socket string
	prefix string
	http   *http.Client
	mu     sync.Mutex
	n      int
}

func newClient(socket, prefix string) *client {
	c := &client{socket: socket, prefix: prefix}
	c.http = &http.Client{Timeout: 4 * time.Minute, Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", socket)
		},
		MaxIdleConns: 4,
	}}
	return c
}

var outMu sync.Mutex

func emit(v map[string]any) {
	outMu.Lock()
	defer outMu.Unlock()
	b, _ := json.Marshal(v)
	os.Stdout.Write(append(b, '\n'))
}

func result(id string, pass bool, format string, args ...any) {
	status := "FAIL"
	if pass {
		status = "PASS"
	}
	emit(map[string]any{"kind": "result", "id": id, "status": status, "detail": fmt.Sprintf(format, args...)})
}

func info(key, format string, args ...any) {
	emit(map[string]any{"kind": "info", "key": key, "detail": fmt.Sprintf(format, args...)})
}

func (c *client) reqID() string {
	c.mu.Lock()
	c.n++
	n := c.n
	c.mu.Unlock()
	var b [3]byte
	_, _ = rand.Read(b[:])
	return fmt.Sprintf("%s-%d-%s", c.prefix, n, hex.EncodeToString(b[:]))
}

// call sends a JSON request. lines is how many audit lines the server owes
// for it: 0 when no response came back.
func (c *client) call(method, path string, body any) (int, []byte, error) {
	id := c.reqID()
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return 0, nil, err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, "http://tenant"+path, rd)
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("X-S06-Req", id)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		emit(map[string]any{"kind": "req", "req": id, "method": method, "status": 0, "lines": 0})
		return 0, nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	emit(map[string]any{"kind": "req", "req": id, "method": method, "status": resp.StatusCode, "lines": 1})
	return resp.StatusCode, data, err
}

type sandboxView struct {
	ID     string `json:"id"`
	Status string `json:"status"`
}

func (c *client) create(body any) (int, string) {
	st, data, err := c.call(http.MethodPost, "/v1/tenant/sandboxes", body)
	if err != nil || st != http.StatusCreated {
		return st, ""
	}
	var sb sandboxView
	_ = json.Unmarshal(data, &sb)
	return st, sb.ID
}

func (c *client) list() ([]sandboxView, error) {
	st, data, err := c.call(http.MethodGet, "/v1/tenant/sandboxes", nil)
	if err != nil {
		return nil, err
	}
	if st != http.StatusOK {
		return nil, fmt.Errorf("list: status %d", st)
	}
	var out []sandboxView
	err = json.Unmarshal(data, &out)
	return out, err
}

func (c *client) status(id string) string {
	st, data, err := c.call(http.MethodGet, "/v1/tenant/sandboxes/"+id, nil)
	if err != nil || st != http.StatusOK {
		return fmt.Sprintf("http-%d", st)
	}
	var sb sandboxView
	_ = json.Unmarshal(data, &sb)
	return sb.Status
}

// execConn is the client end of an exec stream.
type execConn struct {
	conn net.Conn
	br   *bufio.Reader
	wmu  sync.Mutex
}

func (e *execConn) send(typ byte, p []byte) error {
	e.wmu.Lock()
	defer e.wmu.Unlock()
	return writeFrame(e.conn, typ, p)
}

func (e *execConn) recv() (byte, []byte, error) { return readFrame(e.br) }
func (e *execConn) close()                      { e.conn.Close() }

// openExec requests an exec. It returns a stream on 101, else the status
// and the error body.
func (c *client) openExec(id string, spec any) (*execConn, int, []byte, string, error) {
	rid := c.reqID()
	body, err := json.Marshal(spec)
	if err != nil {
		return nil, 0, nil, rid, err
	}
	conn, err := net.Dial("unix", c.socket)
	if err != nil {
		emit(map[string]any{"kind": "req", "req": rid, "method": "EXEC", "status": 0, "lines": 0})
		return nil, 0, nil, rid, err
	}
	req, _ := http.NewRequest(http.MethodPost, "http://tenant/v1/tenant/sandboxes/"+id+"/exec", bytes.NewReader(body))
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", execProtocol)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-S06-Req", rid)
	conn.SetDeadline(time.Now().Add(3 * time.Minute))
	if err := req.Write(conn); err != nil {
		conn.Close()
		emit(map[string]any{"kind": "req", "req": rid, "method": "EXEC", "status": 0, "lines": 0})
		return nil, 0, nil, rid, err
	}
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, req)
	if err != nil {
		conn.Close()
		emit(map[string]any{"kind": "req", "req": rid, "method": "EXEC", "status": 0, "lines": 0})
		return nil, 0, nil, rid, err
	}
	if resp.StatusCode != http.StatusSwitchingProtocols {
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		conn.Close()
		emit(map[string]any{"kind": "req", "req": rid, "method": "EXEC", "status": resp.StatusCode, "lines": 1})
		return nil, resp.StatusCode, data, rid, nil
	}
	conn.SetDeadline(time.Time{})
	emit(map[string]any{"kind": "req", "req": rid, "method": "EXEC", "status": 101, "lines": 2})
	return &execConn{conn: conn, br: br}, 101, nil, rid, nil
}

type execOut struct {
	status         int
	body           []byte
	stdout, stderr bytes.Buffer
	errs           string
	exit           *ExitInfo
	err            error
	firstErr       time.Duration
	elapsed        time.Duration
	reqID          string
}

// exec runs spec to the end: it streams stdin, then waits for the exit
// frame. onStdout, when set, receives stdout instead of the buffer.
func (c *client) exec(id string, spec ExecSpec, stdin io.Reader, onStdout io.Writer) *execOut {
	o := &execOut{}
	t0 := time.Now()
	ec, st, body, rid, err := c.openExec(id, spec)
	o.status, o.body, o.reqID, o.err = st, body, rid, err
	if ec == nil {
		return o
	}
	defer ec.close()
	ec.conn.SetDeadline(time.Now().Add(time.Duration(spec.TimeoutSeconds)*time.Second + time.Minute))
	if spec.Stdin {
		go func() {
			buf := make([]byte, 256<<10)
			for stdin != nil {
				n, err := stdin.Read(buf)
				if n > 0 && ec.send(frameStdin, buf[:n]) != nil {
					return
				}
				if err != nil {
					break
				}
			}
			_ = ec.send(frameStdinEOF, nil)
		}()
	}
	for {
		typ, p, err := ec.recv()
		if err != nil {
			o.err = fmt.Errorf("stream ended without an exit frame: %w", err)
			break
		}
		switch typ {
		case frameStdout:
			if onStdout != nil {
				_, _ = onStdout.Write(p)
			} else {
				o.stdout.Write(p)
			}
		case frameStderr:
			o.stderr.Write(p)
		case frameError:
			if o.errs == "" {
				o.firstErr = time.Since(t0)
			}
			o.errs += string(p)
		case frameExit:
			var ei ExitInfo
			if err := json.Unmarshal(p, &ei); err != nil {
				o.err = err
			}
			o.exit = &ei
			o.elapsed = time.Since(t0)
			return o
		}
	}
	o.elapsed = time.Since(t0)
	return o
}

func (o *execOut) code() int {
	if o.exit == nil || o.exit.Code == nil {
		return -1
	}
	return *o.exit.Code
}

func (o *execOut) String() string {
	reason := ""
	if o.exit != nil {
		reason = o.exit.Reason
	}
	return fmt.Sprintf("http=%d code=%d reason=%s err=%v", o.status, o.code(), reason, o.err)
}

func runClient(args []string) error {
	if len(args) < 1 {
		return errors.New("client needs a scenario")
	}
	fs := flag.NewFlagSet("client", flag.ContinueOnError)
	socket := fs.String("socket", "/run/wsm/tenant.sock", "tenant socket")
	target := fs.String("target", "", "a sandbox ID of another tenant")
	image := fs.String("image", "", "the project's image")
	cpu := fs.Int64("cpu-millis", 0, "the project's cpu limit")
	mem := fs.Int64("mem-bytes", 0, "the project's memory limit")
	pids := fs.Int64("pids-limit", 0, "the project's pids limit")
	count := fs.Int("count", 20, "restart-probe attempts")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	c := newClient(*socket, args[0])
	switch args[0] {
	case "whoami":
		st, data, err := c.call(http.MethodGet, "/v1/tenant/whoami", nil)
		v := map[string]any{"kind": "whoami", "status": st}
		if err != nil {
			v["error"] = err.Error()
		} else {
			v["body"] = json.RawMessage(data)
		}
		emit(v)
	case "probe-denied":
		c.probeDenied()
	case "api":
		c.api(*image, *cpu, *mem, *pids)
	case "cross":
		c.cross(*target)
	case "create-once":
		st, id := c.create(map[string]any{})
		emit(map[string]any{"kind": "create", "status": st, "id": id})
	case "cleanup":
		sbs, err := c.list()
		n := 0
		for _, sb := range sbs {
			if st, _, _ := c.call(http.MethodDelete, "/v1/tenant/sandboxes/"+sb.ID, nil); st == http.StatusNoContent {
				n++
			}
		}
		emit(map[string]any{"kind": "cleanup", "deleted": n, "error": fmt.Sprint(err)})
	case "exec":
		c.execScenario()
	case "restart-probe":
		for i := range *count {
			st, _, err := c.call(http.MethodGet, "/v1/tenant/whoami", nil)
			emit(map[string]any{"kind": "probe-step", "i": i, "status": st, "error": fmt.Sprint(err)})
			time.Sleep(2 * time.Second)
		}
	default:
		return fmt.Errorf("unknown scenario %q", args[0])
	}
	return nil
}

// probeDenied tries every kind of call from a peer that must be refused.
func (c *client) probeDenied() {
	var codes []int
	for _, r := range []struct {
		method, path string
		body         any
	}{
		{http.MethodGet, "/v1/tenant/whoami", nil},
		{http.MethodGet, "/v1/tenant/sandboxes", nil},
		{http.MethodPost, "/v1/tenant/sandboxes", map[string]any{}},
		{http.MethodDelete, "/v1/tenant/sandboxes/tzzzzzzzzzz", nil},
	} {
		st, _, err := c.call(r.method, r.path, r.body)
		if err != nil {
			st = 0
		}
		codes = append(codes, st)
	}
	_, st, _, _, err := c.openExec("tzzzzzzzzzz", ExecSpec{Argv: []string{"true"}, TimeoutSeconds: 5})
	if err != nil {
		st = 0
	}
	codes = append(codes, st)
	emit(map[string]any{"kind": "probe", "statuses": codes})
}

func (c *client) count() int {
	sbs, err := c.list()
	if err != nil {
		return -1
	}
	return len(sbs)
}

func (c *client) api(image string, cpu, mem, pids int64) {
	// CEIL-IMAGE
	before := c.count()
	st, _ := c.create(map[string]any{"image": "docker.io/library/alpine:3.20"})
	after := c.count()
	result("CEIL-IMAGE", st == http.StatusForbidden && before == after && before >= 0,
		"other image: %d, sandboxes before %d after %d", st, before, after)
	if image != "" {
		st, id := c.create(map[string]any{"image": image})
		info("create-own-image", "the project's own image: %d", st)
		if id != "" {
			c.call(http.MethodDelete, "/v1/tenant/sandboxes/"+id, nil)
		}
	}

	// CEIL-LIMITS
	var codes []int
	for _, body := range []map[string]any{{"cpu_millis": cpu + 1}, {"mem_bytes": mem + 1}, {"pids_limit": pids + 1}} {
		st, id := c.create(body)
		codes = append(codes, st)
		if id != "" {
			c.call(http.MethodDelete, "/v1/tenant/sandboxes/"+id, nil)
		}
	}
	result("CEIL-LIMITS", cpu > 0 && mem > 0 && pids > 0 && slices.Equal(codes, []int{403, 403, 403}),
		"cpu+1, mem+1, pids+1: %v (limits %d/%d/%d)", codes, cpu, mem, pids)

	// TENANT-OWN
	t0 := time.Now()
	st, id := c.create(map[string]any{})
	info("create-time", "first create: %d ms", time.Since(t0).Milliseconds())
	var steps []string
	ok := st == http.StatusCreated
	steps = append(steps, fmt.Sprintf("create=%d", st))
	if ok {
		sbs, _ := c.list()
		inList := slices.ContainsFunc(sbs, func(s sandboxView) bool { return s.ID == id })
		s1 := c.status(id)
		st1, _, _ := c.call(http.MethodPost, "/v1/tenant/sandboxes/"+id+"/stop", nil)
		s2 := c.status(id)
		st2, _, _ := c.call(http.MethodPost, "/v1/tenant/sandboxes/"+id+"/start", nil)
		s3 := c.status(id)
		var ex *execOut
		for range 30 {
			ex = c.exec(id, ExecSpec{Argv: []string{"true"}, TimeoutSeconds: 10}, nil, nil)
			if ex.code() == 0 {
				break
			}
			time.Sleep(time.Second)
		}
		st3, _, _ := c.call(http.MethodDelete, "/v1/tenant/sandboxes/"+id, nil)
		sbs, _ = c.list()
		gone := !slices.ContainsFunc(sbs, func(s sandboxView) bool { return s.ID == id })
		steps = append(steps, fmt.Sprintf("list=%v get=%s stop=%d get=%s start=%d get=%s exec=%d delete=%d gone=%v",
			inList, s1, st1, s2, st2, s3, ex.code(), st3, gone))
		ok = inList && s1 == "running" && st1 == 204 && s2 == "stopped" && st2 == 204 && s3 == "running" &&
			ex.code() == 0 && st3 == 204 && gone
	}
	result("TENANT-OWN", ok, "%s", strings.Join(steps, " "))
	emit(map[string]any{"kind": "own", "id": id})

	// CEIL-COUNT
	stA, id1 := c.create(map[string]any{})
	stB, id2 := c.create(map[string]any{})
	stC, _ := c.create(map[string]any{})
	stD, _, _ := c.call(http.MethodDelete, "/v1/tenant/sandboxes/"+id1, nil)
	stE, id3 := c.create(map[string]any{})
	result("CEIL-COUNT", stA == 201 && stB == 201 && stC == 403 && stD == 204 && stE == 201,
		"create %d %d, third %d, delete %d, create again %d", stA, stB, stC, stD, stE)

	// SPEC-STRICT, against a running sandbox of the tenant's own.
	args65 := make([]string, 65)
	for i := range args65 {
		args65[i] = "x"
	}
	bad := []any{
		map[string]any{"argv": []string{"true"}, "timeout_seconds": 5, "user": "root"},
		map[string]any{"argv": []string{"true"}, "timeout_seconds": 5, "env": map[string]string{"1A": "x"}},
		map[string]any{"argv": []string{"tr", "a\x00b"}, "timeout_seconds": 5},
		map[string]any{"argv": []string{"-x"}, "timeout_seconds": 5},
		map[string]any{"argv": args65, "timeout_seconds": 5},
		map[string]any{"argv": []string{"true"}, "timeout_seconds": maxTimeout + 1},
	}
	var specCodes []int
	for _, b := range bad {
		ec, st, _, _, _ := c.openExec(id2, b)
		if ec != nil {
			ec.close()
		}
		specCodes = append(specCodes, st)
	}
	result("SPEC-STRICT", slices.Equal(specCodes, []int{400, 400, 400, 400, 400, 400}),
		"unknown field, env name, NUL, argv[0] '-', 65 args, timeout: %v", specCodes)

	if id3 != "" {
		c.call(http.MethodDelete, "/v1/tenant/sandboxes/"+id3, nil)
	}
	emit(map[string]any{"kind": "keep", "id": id2})
}

// cross uses another tenant's sandbox ID through this tenant's socket.
func (c *client) cross(target string) {
	random := "tzzzzzzzzzz"
	var notes []string
	ok := sandboxIDRE.MatchString(target)
	for _, r := range []struct{ method, suffix string }{
		{http.MethodGet, ""}, {http.MethodPost, "/start"}, {http.MethodPost, "/stop"}, {http.MethodDelete, ""},
	} {
		st1, b1, _ := c.call(r.method, "/v1/tenant/sandboxes/"+target+r.suffix, nil)
		st2, b2, _ := c.call(r.method, "/v1/tenant/sandboxes/"+random+r.suffix, nil)
		same := st1 == http.StatusNotFound && st1 == st2 && bytes.Equal(b1, b2)
		ok = ok && same
		notes = append(notes, fmt.Sprintf("%s%s=%d/%d", r.method, r.suffix, st1, st2))
	}
	spec := ExecSpec{Argv: []string{"true"}, TimeoutSeconds: 5}
	ec1, st1, b1, _, _ := c.openExec(target, spec)
	ec2, st2, b2, _, _ := c.openExec(random, spec)
	for _, ec := range []*execConn{ec1, ec2} {
		if ec != nil {
			ec.close()
		}
	}
	ok = ok && st1 == http.StatusNotFound && st1 == st2 && bytes.Equal(b1, b2)
	notes = append(notes, fmt.Sprintf("exec=%d/%d", st1, st2))
	sbs, err := c.list()
	listed := slices.ContainsFunc(sbs, func(s sandboxView) bool { return s.ID == target })
	ok = ok && err == nil && !listed
	notes = append(notes, fmt.Sprintf("in-own-list=%v", listed))
	result("TENANT-CROSS", ok, "%s (target vs random id)", strings.Join(notes, " "))
}

// countProcs counts processes in the sandbox whose command line (NULs as
// spaces) is exactly want plus a space, or starts with want when prefix.
func (c *client) countProcs(id, want string, prefix bool) int {
	pat := `"$1 "`
	if prefix {
		pat = `"$1"*`
	}
	script := `n=0; for f in /proc/[0-9]*/cmdline; do c=$(tr '\0' ' ' < "$f" 2>/dev/null); case $c in ` + pat +
		`) n=$((n + 1));; esac; done; echo "$n"`
	o := c.exec(id, ExecSpec{Argv: []string{"sh", "-c", script, "s06-count", want}, TimeoutSeconds: 30}, nil, nil)
	n, err := strconv.Atoi(strings.TrimSpace(o.stdout.String()))
	if err != nil || o.code() != 0 {
		return -1
	}
	return n
}

func (c *client) waitNoProcs(id, want string, prefix bool, within time.Duration) (int, time.Duration) {
	t0 := time.Now()
	for {
		n := c.countProcs(id, want, prefix)
		if n == 0 || time.Since(t0) > within {
			return n, time.Since(t0)
		}
		time.Sleep(500 * time.Millisecond)
	}
}

func sha256Hex(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

func (c *client) execScenario() {
	senv, sargv := os.Getenv("S06_SENTINEL_ENV"), os.Getenv("S06_SENTINEL_ARGV")
	st, id := c.create(map[string]any{})
	if id == "" || senv == "" || sargv == "" {
		for _, r := range []string{"EXEC-EXIT", "EXEC-STDIN", "EXEC-ENV", "EXEC-TIMEOUT", "EXEC-CANCEL", "EXEC-PARALLEL",
			"EXEC-LONG", "EXEC-IDLE", "EXEC-LIMIT", "DUPLEX-ECHO", "DUPLEX-STOP", "XFER", "SANDBOX-NO-TENANT"} {
			result(r, false, "no sandbox (create %d) or no sentinels", st)
		}
		return
	}
	defer c.call(http.MethodDelete, "/v1/tenant/sandboxes/"+id, nil)

	// EXEC-EXIT
	o := c.exec(id, ExecSpec{Argv: []string{"sh", "-c", "echo out; echo err >&2; exit 7"}, TimeoutSeconds: 30}, nil, nil)
	result("EXEC-EXIT", o.stdout.String() == "out\n" && o.stderr.String() == "err\n" && o.code() == 7,
		"stdout %q stderr %q %s", o.stdout.String(), o.stderr.String(), o)

	// EXEC-STDIN
	data := make([]byte, 8<<20)
	_, _ = rand.Read(data)
	o = c.exec(id, ExecSpec{Argv: []string{"sha256sum"}, Stdin: true, TimeoutSeconds: 120}, bytes.NewReader(data), nil)
	got, _, _ := strings.Cut(o.stdout.String(), " ")
	result("EXEC-STDIN", got == sha256Hex(data) && o.code() == 0, "8 MiB: sha256 equal=%v %s", got == sha256Hex(data), o)

	// EXEC-ENV: the multi-line value carries the env sentinel, so the run's
	// scanner can look for it in host argv.
	v1 := "first line\n" + senv + "\nk=v \"double\" 'single' $HOME `tick`\n"
	o = c.exec(id, ExecSpec{
		Argv:           []string{"sh", "-c", `printf %s "$S06_V1" | sha256sum; printf %s "$S06_V2" | wc -c`},
		Env:            map[string]string{"S06_V1": v1, "S06_V2": ""},
		TimeoutSeconds: 30,
	}, nil, nil)
	lines := strings.Split(strings.TrimSpace(o.stdout.String()), "\n")
	envOK := len(lines) == 2 && strings.HasPrefix(lines[0], sha256Hex([]byte(v1))) && strings.TrimSpace(lines[1]) == "0"
	result("EXEC-ENV", envOK && o.code() == 0, "multi-line value sha256 equal and empty value empty: %v %s (host argv: see the scan)", envOK, o)

	// EXEC-TIMEOUT
	o = c.exec(id, ExecSpec{Argv: []string{"sleep", "30"}, TimeoutSeconds: 3}, nil, nil)
	left := c.countProcs(id, "sleep 30", false)
	result("EXEC-TIMEOUT", o.exit != nil && o.exit.TimedOut && o.code() == 124 && o.elapsed < 5*time.Second && left == 0,
		"elapsed %d ms, %s, sleep 30 left: %d", o.elapsed.Milliseconds(), o, left)

	// EXEC-CANCEL
	ec, st, _, rid, err := c.openExec(id, ExecSpec{Argv: []string{"sleep", "300"}, TimeoutSeconds: 600})
	if ec != nil {
		time.Sleep(2 * time.Second)
		ec.close()
		n, took := c.waitNoProcs(id, "sleep 300", false, 15*time.Second)
		emit(map[string]any{"kind": "audit-expect", "req": rid, "reason": "cancel"})
		result("EXEC-CANCEL", n == 0, "sleep 300 gone after %d ms (left %d)", took.Milliseconds(), n)
	} else {
		result("EXEC-CANCEL", false, "exec: %d %v", st, err)
	}

	// EXEC-PARALLEL
	t0 := time.Now()
	var wg sync.WaitGroup
	codes := make([]int, 8)
	for i := range codes {
		wg.Add(1)
		go func() {
			defer wg.Done()
			codes[i] = c.exec(id, ExecSpec{Argv: []string{"sleep", "3"}, TimeoutSeconds: 30}, nil, nil).code()
		}()
	}
	wg.Wait()
	took := time.Since(t0)
	allZero := !slices.ContainsFunc(codes, func(x int) bool { return x != 0 })
	result("EXEC-PARALLEL", allZero && took <= 6*time.Second, "8 x sleep 3: codes %v in %d ms", codes, took.Milliseconds())

	// EXEC-LONG, EXEC-IDLE and an exec with the argv sentinel, together.
	var long, idle, argvRun *execOut
	wg.Add(3)
	go func() {
		defer wg.Done()
		long = c.exec(id, ExecSpec{Argv: []string{"sh", "-c",
			`i=0; while [ "$i" -lt 120 ]; do echo "line $i"; sleep 1; i=$((i + 1)); done`}, TimeoutSeconds: 300}, nil, nil)
	}()
	go func() {
		defer wg.Done()
		idle = c.exec(id, ExecSpec{Argv: []string{"sh", "-c", "sleep 150; echo done"}, TimeoutSeconds: 300}, nil, nil)
	}()
	go func() {
		defer wg.Done()
		argvRun = c.exec(id, ExecSpec{Argv: []string{"sh", "-c", "sleep 20", "s06-argv", sargv}, TimeoutSeconds: 60}, nil, nil)
	}()
	wg.Wait()
	n := strings.Count(long.stdout.String(), "\n")
	result("EXEC-LONG", n == 120 && long.code() == 0, "%d lines in %d ms, %s", n, long.elapsed.Milliseconds(), long)
	result("EXEC-IDLE", idle.stdout.String() == "done\n" && idle.code() == 0, "stdout %q after %d ms, %s",
		idle.stdout.String(), idle.elapsed.Milliseconds(), idle)
	info("argv-exec", "the exec with the argv sentinel: %s", argvRun)

	// EXEC-LIMIT: yes never ends by itself, so only the kill stops it.
	o = c.exec(id, ExecSpec{Argv: []string{"yes"}, StdoutLimit: 1 << 20, TimeoutSeconds: 120}, nil, io.Discard)
	left, tookLeft := c.waitNoProcs(id, "yes", false, 15*time.Second)
	result("EXEC-LIMIT", strings.Contains(o.errs, "limit of 1048576 bytes") && o.firstErr < 2*time.Second && left == 0,
		"error %q after %d ms, %s, yes gone after %d ms (left %d)", o.errs, o.firstErr.Milliseconds(), o, tookLeft.Milliseconds(), left)

	c.duplexEcho(id)
	c.duplexStop(id)
	c.xfer(id)

	// exec latency
	var lat []time.Duration
	for range 50 {
		o := c.exec(id, ExecSpec{Argv: []string{"true"}, TimeoutSeconds: 10}, nil, nil)
		if o.code() == 0 {
			lat = append(lat, o.elapsed)
		}
	}
	slices.Sort(lat)
	if len(lat) > 0 {
		info("exec-latency", "%d x true: median %d ms, p95 %d ms, min %d ms, max %d ms", len(lat),
			lat[len(lat)/2].Milliseconds(), lat[len(lat)*95/100].Milliseconds(), lat[0].Milliseconds(), lat[len(lat)-1].Milliseconds())
	}

	// SANDBOX-NO-TENANT
	o = c.exec(id, ExecSpec{Argv: []string{"sh", "-c",
		`if [ -e /run/s06 ]; then echo run-s06-present; fi; find / -path /proc -prune -o -type s -print 2>/dev/null`},
		TimeoutSeconds: 60}, nil, nil)
	out := o.stdout.String()
	socks := strings.Fields(out)
	result("SANDBOX-NO-TENANT", o.exit != nil && !strings.Contains(out, "run-s06-present") && !strings.Contains(out, "tenant"),
		"/run/s06 absent, %d sockets found, none for a tenant: %v", len(socks), socks)
}

func (c *client) duplexEcho(id string) {
	ec, st, _, _, err := c.openExec(id, ExecSpec{Argv: []string{"cat"}, Stdin: true, TimeoutSeconds: 300})
	if ec == nil {
		result("DUPLEX-ECHO", false, "exec: %d %v", st, err)
		return
	}
	defer ec.close()
	ec.conn.SetDeadline(time.Now().Add(5 * time.Minute))
	chunk := make([]byte, 1024)
	var rtts []time.Duration
	ok := true
	var pending []byte
	for i := 0; i < 1000 && ok; i++ {
		_, _ = rand.Read(chunk)
		t0 := time.Now()
		if err := ec.send(frameStdin, chunk); err != nil {
			ok = false
			break
		}
		for len(pending) < len(chunk) {
			typ, p, err := ec.recv()
			if err != nil || typ != frameStdout {
				ok = false
				break
			}
			pending = append(pending, p...)
		}
		if !ok || !bytes.Equal(pending[:len(chunk)], chunk) {
			ok = false
			break
		}
		pending = pending[len(chunk):]
		rtts = append(rtts, time.Since(t0))
	}
	_ = ec.send(frameStdinEOF, nil)
	var exit ExitInfo
	for ok {
		typ, p, err := ec.recv()
		if err != nil {
			ok = false
			break
		}
		if typ == frameExit {
			_ = json.Unmarshal(p, &exit)
			break
		}
	}
	slices.Sort(rtts)
	code := -1
	if exit.Code != nil {
		code = *exit.Code
	}
	median := time.Duration(0)
	if len(rtts) > 0 {
		median = rtts[len(rtts)/2]
	}
	result("DUPLEX-ECHO", ok && len(rtts) == 1000 && code == 0, "%d/1000 echoes exact, exit %d", len(rtts), code)
	info("duplex-rtt", "1 KiB round trip: median %d us", median.Microseconds())
}

func (c *client) duplexStop(id string) {
	const loop = `trap "" TERM; while :; do sleep 1; done`
	ec, st, _, _, err := c.openExec(id, ExecSpec{Argv: []string{"sh", "-c", loop}, TimeoutSeconds: 300})
	if ec == nil {
		result("DUPLEX-STOP", false, "exec: %d %v", st, err)
		return
	}
	defer ec.close()
	time.Sleep(time.Second)
	t0 := time.Now()
	_ = ec.send(frameStop, nil)
	ec.conn.SetDeadline(time.Now().Add(time.Minute))
	var exit ExitInfo
	got := false
	for {
		typ, p, err := ec.recv()
		if err != nil {
			break
		}
		if typ == frameExit {
			_ = json.Unmarshal(p, &exit)
			got = true
			break
		}
	}
	took := time.Since(t0)
	left := c.countProcs(id, "sh -c "+loop, false)
	result("DUPLEX-STOP", got && exit.Stopped && took <= 8*time.Second && left == 0,
		"exit after %d ms: stopped=%v reason=%s, left %d", took.Milliseconds(), exit.Stopped, exit.Reason, left)
}

// xfer moves a 128 MiB file in and out as tar streams, generated and parsed
// on the fly so the client never holds it in memory.
func (c *client) xfer(id string) {
	const size = 128 << 20
	pr, pw := io.Pipe()
	h := sha256.New()
	go func() {
		tw := tar.NewWriter(pw)
		err := tw.WriteHeader(&tar.Header{Name: "data.bin", Mode: 0o644, Size: size, Typeflag: tar.TypeReg})
		if err == nil {
			rng := mrand.NewChaCha8([32]byte{6})
			_, err = io.CopyN(io.MultiWriter(tw, h), rng, size)
		}
		if err == nil {
			err = tw.Close()
		}
		pw.CloseWithError(err)
	}()
	t0 := time.Now()
	in := c.exec(id, ExecSpec{Argv: []string{"sh", "-c", "mkdir -p /tmp/s06x && exec tar -x -C /tmp/s06x"},
		Stdin: true, TimeoutSeconds: 600}, pr, nil)
	inTook := time.Since(t0)
	want := hex.EncodeToString(h.Sum(nil))
	sum := c.exec(id, ExecSpec{Argv: []string{"sha256sum", "/tmp/s06x/data.bin"}, TimeoutSeconds: 120}, nil, nil)
	gotIn, _, _ := strings.Cut(sum.stdout.String(), " ")

	// Out: parse the archive strictly, as the plugin would.
	opr, opw := io.Pipe()
	outHash := make(chan string, 1)
	go func() {
		tr := tar.NewReader(opr)
		hh := sha256.New()
		var n int
		bad := ""
		for {
			hdr, err := tr.Next()
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				bad = err.Error()
				break
			}
			n++
			if hdr.Typeflag != tar.TypeReg || hdr.Name != "data.bin" {
				bad = "unexpected member " + hdr.Name
				break
			}
			if _, err := io.Copy(hh, tr); err != nil {
				bad = err.Error()
				break
			}
		}
		_, _ = io.Copy(io.Discard, opr)
		if bad != "" || n != 1 {
			outHash <- "bad: " + bad
			return
		}
		outHash <- hex.EncodeToString(hh.Sum(nil))
	}()
	t1 := time.Now()
	out := c.exec(id, ExecSpec{Argv: []string{"tar", "-c", "-C", "/tmp/s06x", "data.bin"}, StdoutLimit: 256 << 20,
		TimeoutSeconds: 600}, nil, opw)
	opw.Close()
	outTook := time.Since(t1)
	gotOut := <-outHash
	c.exec(id, ExecSpec{Argv: []string{"rm", "-rf", "/tmp/s06x"}, TimeoutSeconds: 60}, nil, nil)
	result("XFER", gotIn == want && gotOut == want && in.code() == 0 && out.code() == 0,
		"128 MiB in: equal=%v (%s); out: equal=%v (%s)", gotIn == want, in, gotOut == want, out)
	mib := float64(size) / (1 << 20)
	info("xfer-throughput", "in %.0f MiB/s, out %.0f MiB/s", mib/inTook.Seconds(), mib/outTook.Seconds())
}
