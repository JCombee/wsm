package podman

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

// pipeListener is a net.Listener whose connections are in-memory pipes, so
// tests need no socket files.
type pipeListener struct {
	conns chan net.Conn
	done  chan struct{}
	once  sync.Once
}

func newPipeListener() *pipeListener {
	return &pipeListener{conns: make(chan net.Conn), done: make(chan struct{})}
}

func (l *pipeListener) Accept() (net.Conn, error) {
	select {
	case c := <-l.conns:
		return c, nil
	case <-l.done:
		return nil, net.ErrClosed
	}
}

func (l *pipeListener) Close() error {
	l.once.Do(func() { close(l.done) })
	return nil
}

func (l *pipeListener) Addr() net.Addr { return &net.UnixAddr{Name: "pipe", Net: "unix"} }

func (l *pipeListener) Dial(ctx context.Context) (net.Conn, error) {
	client, server := net.Pipe()
	select {
	case l.conns <- server:
		return client, nil
	case <-l.done:
		return nil, net.ErrClosed
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// fakeServer serves h as the Podman API and returns a client for it.
func fakeServer(t *testing.T, h http.Handler) *Client {
	t.Helper()
	ln := newPipeListener()
	srv := &http.Server{Handler: h}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })
	c := NewClientDialer(ln.Dial)
	t.Cleanup(c.Close)
	return c
}

func TestParseAPIError(t *testing.T) {
	tests := []struct {
		name     string
		status   int
		body     string
		wantMsg  string
		notFound bool
	}{
		{"podman body", 404, `{"cause":"no such pod","message":"no pod with name or ID sbx found","response":404}`, "no pod with name or ID sbx found", true},
		{"status in body is ignored", 500, `{"message":"boom","response":200}`, "boom", false},
		{"not json", 500, `<html>`, "Internal Server Error", false},
		{"empty", 409, ``, "Conflict", false},
		{"long message is cut", 500, `{"message":"` + strings.Repeat("x", 2000) + `"}`, strings.Repeat("x", maxErrorMessage) + "...", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := fmt.Errorf("wrapped: %w", parseAPIError(tt.status, []byte(tt.body)))
			var apiErr *APIError
			if !errors.As(err, &apiErr) {
				t.Fatalf("not an APIError: %v", err)
			}
			if apiErr.Status != tt.status || apiErr.Message != tt.wantMsg {
				t.Fatalf("got %d %q, want %d %q", apiErr.Status, apiErr.Message, tt.status, tt.wantMsg)
			}
			if IsNotFound(err) != tt.notFound {
				t.Fatalf("IsNotFound = %v", !tt.notFound)
			}
		})
	}
}

func TestReadLimited(t *testing.T) {
	if _, err := readLimited(strings.NewReader("12345"), 5); err != nil {
		t.Fatalf("at the limit: %v", err)
	}
	if _, err := readLimited(strings.NewReader("123456"), 5); err == nil {
		t.Fatal("over the limit succeeded")
	}
}

func TestEndpoint(t *testing.T) {
	got := endpoint("/exec/abc/resize", map[string][]string{"h": {"37"}, "w": {"123"}})
	want := "http://podman/" + apiVersion + "/libpod/exec/abc/resize?h=37&w=123"
	if got != want {
		t.Fatalf("endpoint = %q, want %q", got, want)
	}
}

func TestResponsesAreUntrusted(t *testing.T) {
	tests := []struct {
		name    string
		body    string
		wantErr string
	}{
		{"valid", `{"Version":"5.4.2"}`, ""},
		{"too large", `{"Version":"` + strings.Repeat("a", MaxResponse) + `"}`, "exceeds"},
		{"trailing data", `{"Version":"5.4.2"} {"Version":"6"}`, "trailing data"},
		{"wrong type", `{"Version":5}`, "decode response"},
		{"not json", `hello`, "decode response"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := fakeServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				io.WriteString(w, tt.body)
			}))
			v, err := c.Version(context.Background())
			if tt.wantErr == "" {
				if err != nil || v != "5.4.2" {
					t.Fatalf("Version = %q, %v", v, err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("err = %v, want %q", err, tt.wantErr)
			}
		})
	}
}

func TestContainerCreateBody(t *testing.T) {
	var got map[string]any
	c := fakeServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/"+apiVersion+"/libpod/containers/create" || r.Method != http.MethodPost {
			http.Error(w, `{"message":"unexpected"}`, 400)
			return
		}
		json.NewDecoder(r.Body).Decode(&got)
		w.WriteHeader(http.StatusCreated)
		io.WriteString(w, `{"Id":"abc","Warnings":[]}`)
	}))
	id, err := c.ContainerCreate(context.Background(), ContainerSpec{
		Name: "sbx-x-work", Image: "img", Pod: "sbx-x", User: "1000:1000",
		Init: true, CapDrop: []string{"ALL"}, NoNewPrivileges: true,
		Volumes: []NamedVolume{{Name: "sbx-x-code", Dest: "/workspace"}},
	})
	if err != nil || id != "abc" {
		t.Fatalf("ContainerCreate = %q, %v", id, err)
	}
	for _, k := range []string{"user", "init", "cap_drop", "no_new_privileges", "volumes", "pod"} {
		if _, ok := got[k]; !ok {
			t.Errorf("body lacks %q: %v", k, got)
		}
	}
	for _, k := range []string{"privileged", "mounts", "devices", "netns", "seccomp_profile_path"} {
		if _, ok := got[k]; ok {
			t.Errorf("body has %q: %v", k, got)
		}
	}
}

func TestRemoveNotFoundIsNoError(t *testing.T) {
	c := fakeServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		io.WriteString(w, `{"message":"no such thing","response":404}`)
	}))
	ctx := context.Background()
	if err := c.PodRemove(ctx, "sbx-x"); err != nil {
		t.Errorf("PodRemove: %v", err)
	}
	if err := c.VolumeRemove(ctx, "sbx-x-code"); err != nil {
		t.Errorf("VolumeRemove: %v", err)
	}
	if ok, err := c.PodExists(ctx, "sbx-x"); ok || err != nil {
		t.Errorf("PodExists = %v, %v", ok, err)
	}
	if ok, err := c.ImageExists(ctx, "img"); ok || err != nil {
		t.Errorf("ImageExists = %v, %v", ok, err)
	}
}

func TestPodActionReportsErrs(t *testing.T) {
	c := fakeServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"Errs":["container failed"],"Id":"x"}`)
	}))
	if err := c.PodStart(context.Background(), "sbx-x"); err == nil || !strings.Contains(err.Error(), "container failed") {
		t.Fatalf("PodStart = %v", err)
	}
}

func TestImagePull(t *testing.T) {
	tests := []struct {
		name    string
		stream  string
		wantErr string
	}{
		{"ok", `{"stream":"pulling"}` + "\n" + `{"id":"abc","images":["abc"]}`, ""},
		{"error object", `{"stream":"pulling"}{"error":"manifest unknown"}`, "manifest unknown"},
		{"garbage", `{"stream":`, "read progress"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := fakeServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Query().Get("reference") != "docker.io/library/x:1" {
					http.Error(w, `{"message":"bad ref"}`, 400)
					return
				}
				io.WriteString(w, tt.stream)
			}))
			err := c.ImagePull(context.Background(), "docker.io/library/x:1")
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

// fakeHijack answers one request on the server end of a pipe with head,
// then writes stream as the hijacked payload.
func fakeHijack(server net.Conn, head, stream string) <-chan *http.Request {
	got := make(chan *http.Request, 1)
	go func() {
		defer server.Close()
		req, err := http.ReadRequest(bufio.NewReader(server))
		if err != nil {
			close(got)
			return
		}
		got <- req
		io.WriteString(server, head+stream)
	}()
	return got
}

func TestUpgrade(t *testing.T) {
	tests := []struct {
		name       string
		head       string
		stream     string
		wantErr    string
		wantStream string
	}{
		{"switching protocols", "HTTP/1.1 101 UPGRADED\r\nContent-Type: application/vnd.docker.raw-stream\r\nConnection: Upgrade\r\nUpgrade: tcp\r\n\r\n", "hello", "", "hello"},
		{"plain 200", "HTTP/1.1 200 OK\r\nContent-Type: application/vnd.docker.raw-stream\r\n\r\n", "hello", "", "hello"},
		{"not found", "HTTP/1.1 404 Not Found\r\nContent-Type: application/json\r\nContent-Length: 34\r\n\r\n", `{"message":"no such exec session"}`, "no such exec session", ""},
		{"conflict", "HTTP/1.1 409 Conflict\r\nContent-Length: 0\r\n\r\n", "", "status 409", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client, server := net.Pipe()
			defer client.Close()
			reqs := fakeHijack(server, tt.head, tt.stream)

			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint("/exec/x/start", nil), strings.NewReader(`{"Tty":true}`))
			if err != nil {
				t.Fatal(err)
			}
			req.Header.Set("Connection", "Upgrade")
			req.Header.Set("Upgrade", "tcp")
			hc, err := upgrade(client, req)

			r := <-reqs
			if r == nil || r.Header.Get("Upgrade") != "tcp" || r.URL.Path != "/"+apiVersion+"/libpod/exec/x/start" {
				t.Fatalf("server saw %+v", r)
			}
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("err = %v, want %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			got, _ := io.ReadAll(hc)
			if string(got) != tt.wantStream {
				t.Fatalf("stream = %q, want %q", got, tt.wantStream)
			}
		})
	}
}
