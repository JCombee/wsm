// Command podmanapi is spike 02: the manager (user wsm) drives a project
// user's rootless Podman over its API socket. It creates a hardened pod,
// checks it from inside, and bridges an exec with a TTY to a WebSocket.
//
// Results go to stdout as "ID<TAB>PASS|FAIL<TAB>detail"; logs go to stderr.
package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/coder/websocket"
)

const (
	podName       = "sbx-spike"
	containerName = "sbx-spike-work"
	sandboxUID    = 1000
)

var resultIDs = []string{"POD-USERNS", "POD-HARDENING", "POD-EXEC-WS", "POD-RESIZE"}

var wantLimits = Limits{CPUQuota: 100000, CPUPeriod: 100000, Memory: 512 << 20, Pids: 256}

type config struct {
	socket  string
	image   string
	hostUID int
}

// reporter writes one result line per criterion.
type reporter struct {
	w    io.Writer
	done map[string]bool
	fail int
}

func (r *reporter) result(id string, problems []string, detail string) {
	status := "PASS"
	if len(problems) > 0 {
		status = "FAIL"
		r.fail++
		detail = strings.Join(problems, "; ")
	}
	r.done[id] = true
	fmt.Fprintf(r.w, "%s\t%s\t%s\n", id, status, detail)
}

// failRest marks every criterion that has no result yet as failed.
func (r *reporter) failRest(reason string) {
	for _, id := range resultIDs {
		if !r.done[id] {
			r.result(id, []string{reason}, "")
		}
	}
}

func main() {
	var cfg config
	flag.StringVar(&cfg.socket, "socket", "", "path to the project user's Podman API socket")
	flag.StringVar(&cfg.image, "image", "docker.io/library/debian:trixie-slim", "workspace image, already pulled")
	flag.IntVar(&cfg.hostUID, "host-uid", -1, "host UID of the project user")
	flag.Parse()
	if cfg.socket == "" || cfg.hostUID < 0 {
		fmt.Fprintln(os.Stderr, "usage: podmanapi -socket PATH -host-uid UID [-image REF]")
		os.Exit(2)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()

	logger := log.New(os.Stderr, "", log.LstdFlags|log.Lmicroseconds)
	rep := &reporter{w: os.Stdout, done: map[string]bool{}}
	if err := run(ctx, cfg, rep, logger); err != nil {
		logger.Printf("error: %v", err)
		rep.failRest(err.Error())
	}
	if rep.fail > 0 {
		os.Exit(1)
	}
}

func run(ctx context.Context, cfg config, rep *reporter, logger *log.Logger) error {
	c := NewClient(cfg.socket)
	v, err := c.Version(ctx)
	if err != nil {
		return err
	}
	logger.Printf("podman %s on %s", v, cfg.socket)

	if err := c.PodRemove(ctx, podName); err != nil && !IsNotFound(err) {
		return err
	}
	if _, err := c.PodCreate(ctx, PodSpec{
		Name:   podName,
		Userns: &Namespace{Mode: "keep-id", Value: fmt.Sprintf("uid=%d,gid=%d", sandboxUID, sandboxUID)},
		Netns:  &Namespace{Mode: "pasta"},
	}); err != nil {
		return err
	}
	if _, err := c.ContainerCreate(ctx, ContainerSpec{
		Name:            containerName,
		Image:           cfg.image,
		Pod:             podName,
		User:            fmt.Sprintf("%d:%d", sandboxUID, sandboxUID),
		Command:         []string{"sleep", "infinity"},
		Init:            true,
		CapDrop:         []string{"ALL"},
		NoNewPrivileges: true,
		Resources: &Resources{
			CPU:    &CPULimit{Quota: wantLimits.CPUQuota, Period: wantLimits.CPUPeriod},
			Memory: &MemoryLimit{Limit: wantLimits.Memory},
			Pids:   &PidsLimit{Limit: wantLimits.Pids},
		},
	}); err != nil {
		return err
	}
	if err := c.PodStart(ctx, podName); err != nil {
		return err
	}
	ci, raw, err := c.ContainerInspect(ctx, containerName)
	if err != nil {
		return err
	}
	logger.Printf("container inspect: %s", raw)
	if raw, err := c.PodInspectRaw(ctx, podName); err == nil {
		logger.Printf("pod inspect: %s", raw)
	}

	rep.result("POD-USERNS", checkUserns(ctx, c, ci, cfg.hostUID, logger),
		fmt.Sprintf("uid %d in the container is host uid %d", sandboxUID, cfg.hostUID))
	rep.result("POD-HARDENING", checkHardening(ctx, c, ci, logger),
		"NoNewPrivs=1, Seccomp=2, no capabilities, uid 1000, cpu/memory/pids limits, no mounts")
	return checkTerminal(ctx, c, rep, logger)
}

func checkUserns(ctx context.Context, c *Client, ci ContainerInspect, hostUID int, logger *log.Logger) []string {
	var problems []string
	logger.Printf("UsernsMode=%q", ci.HostConfig.UsernsMode)
	for _, f := range []string{"uid_map", "gid_map"} {
		out, err := c.Output(ctx, containerName, "cat", "/proc/self/"+f)
		if err != nil {
			problems = append(problems, err.Error())
			continue
		}
		logger.Printf("%s:\n%s", f, out)
		ranges, err := parseIDMap(out)
		if err != nil {
			problems = append(problems, fmt.Sprintf("%s: %v", f, err))
			continue
		}
		// In the rootless user namespace, 0 is the project user itself.
		if !mapsTo(ranges, sandboxUID, 0) {
			problems = append(problems, fmt.Sprintf("%s does not map %d to the project user", f, sandboxUID))
		}
	}
	if ci.State.Pid <= 0 {
		return append(problems, "container has no pid")
	}
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/status", ci.State.Pid))
	if err != nil {
		return append(problems, fmt.Sprintf("read host status of pid %d: %v", ci.State.Pid, err))
	}
	status, err := parseProcStatus(string(data))
	if err != nil {
		return append(problems, err.Error())
	}
	if p := checkIDs(status["Uid"], hostUID); p != "" {
		problems = append(problems, "host Uid "+p)
	}
	return problems
}

func checkHardening(ctx context.Context, c *Client, ci ContainerInspect, logger *log.Logger) []string {
	problems := checkInspectHardening(ci)
	logger.Printf("CapDrop=%q SecurityOpt=%q CpuQuota=%d CpuPeriod=%d Memory=%d PidsLimit=%d",
		ci.HostConfig.CapDrop, ci.HostConfig.SecurityOpt, ci.HostConfig.CPUQuota,
		ci.HostConfig.CPUPeriod, ci.HostConfig.Memory, ci.HostConfig.PidsLimit)

	// pid 1 is the init process; self is the exec itself.
	for _, pid := range []string{"1", "self"} {
		out, err := c.Output(ctx, containerName, "cat", "/proc/"+pid+"/status")
		if err != nil {
			problems = append(problems, err.Error())
			continue
		}
		logger.Printf("/proc/%s/status:\n%s", pid, out)
		status, err := parseProcStatus(out)
		if err != nil {
			problems = append(problems, err.Error())
			continue
		}
		for _, p := range checkProcHardening(status, sandboxUID) {
			problems = append(problems, "pid "+pid+": "+p)
		}
	}

	files := make(map[string]string)
	for _, f := range []string{"cpu.max", "memory.max", "pids.max"} {
		out, err := c.Output(ctx, containerName, "cat", "/sys/fs/cgroup/"+f)
		if err != nil {
			problems = append(problems, err.Error())
		}
		files[f] = out
	}
	logger.Printf("cgroup limits: %q", files)
	problems = append(problems, checkCgroupLimits(files["cpu.max"], files["memory.max"], files["pids.max"], wantLimits)...)

	if out, err := c.Output(ctx, containerName, "cat", "/proc/self/mountinfo"); err == nil {
		logger.Printf("mountinfo (for the findings):\n%s", out)
	}
	return problems
}

// checkTerminal serves the bridge on loopback and drives it like a browser.
func checkTerminal(ctx context.Context, c *Client, rep *reporter, logger *log.Logger) error {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return fmt.Errorf("listen: %w", err)
	}
	mux := http.NewServeMux()
	mux.Handle("/term", &Bridge{Podman: c, Container: containerName, Cmd: []string{"bash", "--norc", "-i"}, Logf: logger.Printf})
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go srv.Serve(ln)
	defer srv.Close()

	ws, _, err := websocket.Dial(ctx, "ws://"+ln.Addr().String()+"/term", nil)
	if err != nil {
		return fmt.Errorf("dial bridge: %w", err)
	}
	defer ws.CloseNow()
	term := newTermClient(ctx, ws)
	defer func() { logger.Printf("terminal transcript: %q", term.transcript()) }()

	// The echoed command contains "$(...)", so only real output matches.
	send := func(line string) error {
		return ws.Write(ctx, websocket.MessageBinary, []byte(line+"\r"))
	}
	wait := func(re string) (string, error) {
		wctx, cancel := context.WithTimeout(ctx, 15*time.Second)
		defer cancel()
		return term.expect(wctx, regexp.MustCompile(re))
	}

	var problems []string
	if err := send(`echo "WS:wsm-$((6*7))"`); err != nil {
		problems = append(problems, err.Error())
	} else if _, err := wait(`WS:wsm-42`); err != nil {
		problems = append(problems, "no command output over the websocket: "+err.Error())
	}
	rep.result("POD-EXEC-WS", problems, "command output came back over the websocket")

	problems = nil
	const rows, cols = 37, 123
	if err := send(`echo "S1:$(stty size)"`); err != nil {
		problems = append(problems, err.Error())
	} else if m, err := wait(`S1:\d+ \d+`); err != nil {
		problems = append(problems, "initial stty size: "+err.Error())
	} else {
		logger.Printf("initial size: %s", m)
	}
	resize := fmt.Sprintf(`{"type":"resize","rows":%d,"cols":%d}`, rows, cols)
	if err := ws.Write(ctx, websocket.MessageText, []byte(resize)); err != nil {
		problems = append(problems, err.Error())
	}
	// The resize is applied before the next input is read, so no sleep.
	if err := send(`echo "S2:$(stty size)"`); err != nil {
		problems = append(problems, err.Error())
	} else if _, err := wait(`S2:` + strconv.Itoa(rows) + ` ` + strconv.Itoa(cols) + `\b`); err != nil {
		problems = append(problems, fmt.Sprintf("stty size after resize is not %d %d: %v", rows, cols, err))
	}
	rep.result("POD-RESIZE", problems, fmt.Sprintf("stty size shows %d %d after a resize message", rows, cols))

	send("exit")
	ws.Close(websocket.StatusNormalClosure, "")
	return nil
}

// maxTranscript caps how much terminal output the client keeps.
const maxTranscript = 1 << 20

// termClient collects terminal output in the background. coder/websocket
// closes the connection when a Read context ends, so reads never time out;
// expect waits on its own context instead.
type termClient struct {
	mu     sync.Mutex
	buf    bytes.Buffer
	notify chan struct{}
	done   chan struct{}
	err    error
}

func newTermClient(ctx context.Context, ws *websocket.Conn) *termClient {
	t := &termClient{notify: make(chan struct{}, 1), done: make(chan struct{})}
	go func() {
		defer close(t.done)
		for {
			_, data, err := ws.Read(ctx)
			if err != nil {
				t.err = err
				return
			}
			t.mu.Lock()
			if t.buf.Len()+len(data) <= maxTranscript {
				t.buf.Write(data)
			}
			t.mu.Unlock()
			select {
			case t.notify <- struct{}{}:
			default:
			}
		}
	}()
	return t
}

func (t *termClient) transcript() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.buf.String()
}

func (t *termClient) expect(ctx context.Context, re *regexp.Regexp) (string, error) {
	for {
		if m := re.FindString(t.transcript()); m != "" {
			return m, nil
		}
		select {
		case <-t.notify:
		case <-t.done:
			if m := re.FindString(t.transcript()); m != "" {
				return m, nil
			}
			return "", fmt.Errorf("connection closed: %w", t.err)
		case <-ctx.Done():
			return "", errors.New("timed out waiting for " + re.String())
		}
	}
}
