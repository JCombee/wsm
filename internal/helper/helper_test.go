package helper

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/JCombee/wsm/internal/projectuid"
)

// fakeOps records which operations ran.
type fakeOps struct {
	mu    sync.Mutex
	calls []string
}

func (f *fakeOps) record(s string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, s)
	return nil
}

func (f *fakeOps) CreateProjectUser(_ context.Context, uid int) error {
	return f.record("create " + UserName(uid))
}
func (f *fakeOps) DeleteProjectUser(_ context.Context, uid int) error {
	return f.record("delete " + UserName(uid))
}
func (f *fakeOps) SetupWSM0(context.Context) error      { return f.record("wsm0") }
func (f *fakeOps) ApplyBaseRules(context.Context) error { return f.record("rules") }
func (f *fakeOps) PullServiceImage(_ context.Context, service, tag string) error {
	return f.record("pull " + service + " " + tag)
}
func (f *fakeOps) EnsureServiceInstance(_ context.Context, service, tag string, port int, _ string) error {
	return f.record(fmt.Sprintf("ensure %s %s %d", service, tag, port))
}
func (f *fakeOps) AllowService(_ context.Context, uid, port int) error {
	return f.record(fmt.Sprintf("allow %d %d", uid, port))
}
func (f *fakeOps) RevokeService(_ context.Context, uid, port int) error {
	return f.record(fmt.Sprintf("revoke %d %d", uid, port))
}

func (f *fakeOps) Calls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

// connPair returns two connected unix sockets. socketpair needs no path,
// so it also works where creating socket files is not allowed; the kernel
// still reports peer credentials for it.
func connPair(t *testing.T) (client, server *net.UnixConn) {
	t.Helper()
	fds, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_STREAM|syscall.SOCK_CLOEXEC, 0)
	if err != nil {
		t.Fatalf("socketpair: %v", err)
	}
	conns := make([]*net.UnixConn, 2)
	for i, fd := range fds {
		f := os.NewFile(uintptr(fd), "socketpair")
		c, err := net.FileConn(f)
		f.Close()
		if err != nil {
			t.Fatalf("file conn: %v", err)
		}
		conns[i] = c.(*net.UnixConn)
	}
	return conns[0], conns[1]
}

// serveOne serves one connection on a server for allowedUID and returns the
// client end, plus a function that waits until the server is done.
func serveOne(t *testing.T, srv *Server) (*net.UnixConn, func()) {
	t.Helper()
	client, server := connPair(t)
	done := make(chan struct{})
	go func() {
		defer close(done)
		srv.ServeConn(context.Background(), server)
	}()
	return client, func() {
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Fatal("server did not finish")
		}
	}
}

// rawExchange sends raw bytes and returns the raw response.
func rawExchange(t *testing.T, srv *Server, msg []byte) string {
	t.Helper()
	uc, wait := serveOne(t, srv)
	defer wait()
	defer uc.Close()
	uc.SetDeadline(time.Now().Add(10 * time.Second))
	// The server may answer and close before it read everything; a write
	// error then is fine, the response is what counts.
	uc.Write(msg)
	uc.CloseWrite()
	resp, err := io.ReadAll(uc)
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	return string(resp)
}

// clientDo sends req the way Client.Do does, over a socketpair.
func clientDo(t *testing.T, srv *Server, req Request) error {
	t.Helper()
	if err := req.Validate(); err != nil {
		return err
	}
	data, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	uc, wait := serveOne(t, srv)
	defer wait()
	defer uc.Close()
	return exchange(context.Background(), uc, req, data)
}

func TestRejectsInvalidRequests(t *testing.T) {
	ops := &fakeOps{}
	srv := &Server{Ops: ops, AllowedUID: uint32(os.Getuid()), Logf: t.Logf}
	tests := []struct {
		name string
		msg  string
	}{
		{"uid below range", `{"type":"create_project_user","uid":49999}`},
		{"uid above range", `{"type":"create_project_user","uid":60000}`},
		{"uid zero", `{"type":"delete_project_user","uid":0}`},
		{"negative uid", `{"type":"delete_project_user","uid":-50000}`},
		{"uid as string", `{"type":"create_project_user","uid":"50000"}`},
		{"fractional uid", `{"type":"create_project_user","uid":50000.5}`},
		{"null uid", `{"type":"create_project_user","uid":null}`},
		{"missing uid", `{"type":"create_project_user"}`},
		{"uid where none is taken", `{"type":"setup_wsm0","uid":50000}`},
		{"unknown type", `{"type":"run_command","uid":50000}`},
		{"empty type", `{"type":"","uid":50000}`},
		{"missing type", `{"uid":50000}`},
		{"extra field path", `{"type":"create_project_user","uid":50000,"path":"/etc"}`},
		{"extra field name", `{"type":"create_project_user","uid":50000,"name":"root"}`},
		{"extra field cmd", `{"type":"apply_base_rules","cmd":["sh"]}`},
		{"field in other case", `{"type":"create_project_user","UID":50000}`},
		{"type in other case", `{"Type":"setup_wsm0"}`},
		{"duplicate field", `{"type":"setup_wsm0","type":"create_project_user","uid":50000}`},
		{"two messages", `{"type":"setup_wsm0"}{"type":"setup_wsm0"}`},
		{"trailing garbage", `{"type":"setup_wsm0"} x`},
		{"not an object", `["setup_wsm0"]`},
		{"not json", `setup_wsm0`},
		{"empty", ``},
		{"too large", `{"type":"setup_wsm0","pad":"` + strings.Repeat("a", MaxMessage) + `"}`},
		{"too large with valid prefix", `{"type":"setup_wsm0"}` + strings.Repeat(" ", MaxMessage)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			before := len(ops.Calls())
			resp := rawExchange(t, srv, []byte(tt.msg))
			if !strings.Contains(resp, `"ok":false`) || !strings.Contains(resp, "invalid request") {
				t.Errorf("response %q, want an invalid-request error", resp)
			}
			if after := ops.Calls(); len(after) != before {
				t.Errorf("operation ran: %v", after[before:])
			}
		})
	}

	// Controls: valid requests do reach the operations.
	lo, hi := projectuid.Min, projectuid.Max
	for _, req := range []Request{
		{Type: TypeCreateProjectUser, UID: &lo},
		{Type: TypeDeleteProjectUser, UID: &hi},
		{Type: TypeSetupWSM0},
		{Type: TypeApplyBaseRules},
	} {
		if err := clientDo(t, srv, req); err != nil {
			t.Fatalf("valid %s: %v", req.Type, err)
		}
	}
	want := []string{"create wsm-p-50000", "delete wsm-p-59999", "wsm0", "rules"}
	if got := ops.Calls(); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("calls = %v, want %v", got, want)
	}
}

func TestClientRejectsInvalidRequests(t *testing.T) {
	c := &Client{Socket: filepath.Join(t.TempDir(), "none.sock")}
	bad := 1
	for _, req := range []Request{
		{Type: "unknown"},
		{Type: TypeCreateProjectUser},
		{Type: TypeCreateProjectUser, UID: &bad},
		{Type: TypeSetupWSM0, UID: &bad},
	} {
		if err := c.Do(context.Background(), req); err == nil || strings.Contains(err.Error(), "dial") {
			t.Errorf("Do(%+v) = %v, want a validation error before dialing", req, err)
		}
	}
}

func TestPeerCredRejectsOtherUID(t *testing.T) {
	me := uint32(os.Getuid())

	// The kernel reports our own UID for our own connection.
	a, b := connPair(t)
	got, err := PeerUID(a)
	a.Close()
	b.Close()
	if err != nil {
		t.Fatal(err)
	}
	if got != me {
		t.Fatalf("PeerUID = %d, want %d", got, me)
	}

	// A server for another UID refuses us, even with a valid request.
	ops := &fakeOps{}
	srv := &Server{Ops: ops, AllowedUID: me + 1, Logf: t.Logf}
	resp := rawExchange(t, srv, []byte(`{"type":"setup_wsm0"}`))
	if !strings.Contains(resp, ErrPeerRejected.Error()) {
		t.Errorf("response %q, want %q", resp, ErrPeerRejected)
	}
	if err := clientDo(t, srv, Request{Type: TypeSetupWSM0}); err == nil ||
		!strings.Contains(err.Error(), ErrPeerRejected.Error()) {
		t.Errorf("client error = %v, want %q", err, ErrPeerRejected)
	}
	if calls := ops.Calls(); len(calls) != 0 {
		t.Errorf("operations ran for another uid: %v", calls)
	}

	// Control: the same request from the allowed UID runs.
	srv.AllowedUID = me
	if err := clientDo(t, srv, Request{Type: TypeSetupWSM0}); err != nil {
		t.Fatalf("allowed uid: %v", err)
	}
	if calls := ops.Calls(); len(calls) != 1 {
		t.Errorf("calls = %v, want one", calls)
	}
}

func TestDerivedNames(t *testing.T) {
	tests := []struct {
		uid               int
		user, home, sock  string
		subFirst, subLast int64
	}{
		{50000, "wsm-p-50000", "/home/wsm-p-50000", "/run/wsm-podman/wsm-p-50000/podman.sock", 3276800000, 3276865535},
		{59999, "wsm-p-59999", "/home/wsm-p-59999", "/run/wsm-podman/wsm-p-59999/podman.sock", 3932094464, 3932159999},
	}
	for _, tt := range tests {
		if got := UserName(tt.uid); got != tt.user {
			t.Errorf("UserName(%d) = %q, want %q", tt.uid, got, tt.user)
		}
		if got := HomeDir(tt.uid); got != tt.home {
			t.Errorf("HomeDir(%d) = %q, want %q", tt.uid, got, tt.home)
		}
		if got := PodmanSocket(tt.uid); got != tt.sock {
			t.Errorf("PodmanSocket(%d) = %q, want %q", tt.uid, got, tt.sock)
		}
		first, last := SubIDRange(tt.uid)
		if first != tt.subFirst || last != tt.subLast || last >= 1<<32 {
			t.Errorf("SubIDRange(%d) = %d-%d, want %d-%d", tt.uid, first, last, tt.subFirst, tt.subLast)
		}
	}
}
