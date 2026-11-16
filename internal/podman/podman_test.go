package podman

import (
	"archive/tar"
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"slices"
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
		{"valid", `{"Config":{"Cmd":["sleep","infinity"]}}`, ""},
		{"too large", `{"Config":{"Cmd":["` + strings.Repeat("a", MaxResponse) + `"]}}`, "exceeds"},
		{"trailing data", `{"Config":{}} {"Config":{}}`, "trailing data"},
		{"trailing brace", `{"Config":{}}}`, "trailing data"},
		{"wrong type", `{"Config":{"Cmd":5}}`, "decode response"},
		{"not json", `hello`, "decode response"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := fakeServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				io.WriteString(w, tt.body)
			}))
			cfg, err := c.ImageInspect(context.Background(), "img")
			if tt.wantErr == "" {
				if err != nil || strings.Join(cfg.Cmd, " ") != "sleep infinity" {
					t.Fatalf("ImageInspect = %+v, %v", cfg, err)
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
	// The body holds exactly the fields that were set: nothing else, such
	// as privileged mode, mounts, devices, namespaces or a seccomp profile,
	// can sneak in.
	want := []string{"name", "image", "pod", "user", "init", "cap_drop", "no_new_privileges", "volumes"}
	for _, k := range want {
		if _, ok := got[k]; !ok {
			t.Errorf("body lacks %q: %v", k, got)
		}
	}
	if len(got) != len(want) {
		t.Errorf("body has %d fields, want exactly %v: %v", len(got), want, got)
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
	if ok, err := c.VolumeExists(ctx, "sbx-x-code"); ok || err != nil {
		t.Errorf("VolumeExists = %v, %v", ok, err)
	}
	if ok, err := c.ImageExists(ctx, "img"); ok || err != nil {
		t.Errorf("ImageExists = %v, %v", ok, err)
	}
	if err := c.ContainerRemove(ctx, "sbx-x-redis"); err != nil {
		t.Errorf("ContainerRemove: %v", err)
	}
	if _, err := c.ContainerInspect(ctx, "sbx-x-redis"); !IsNotFound(err) {
		t.Errorf("ContainerInspect of a missing container: %v", err)
	}
}

func TestContainerCalls(t *testing.T) {
	var reqs []string
	c := fakeServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := strings.TrimPrefix(r.URL.Path, "/"+apiVersion+"/libpod")
		reqs = append(reqs, r.Method+" "+path+" "+r.URL.RawQuery)
		switch {
		case path == "/containers/up/start":
			w.WriteHeader(http.StatusNotModified)
		case path == "/containers/down/start":
			w.WriteHeader(http.StatusNoContent)
		case path == "/containers/bad/start":
			w.WriteHeader(http.StatusInternalServerError)
			io.WriteString(w, `{"message":"cannot start"}`)
		case path == "/containers/up" && r.Method == http.MethodDelete:
			io.WriteString(w, `[{"Id":"abc"}]`)
		case path == "/containers/up/json":
			io.WriteString(w, `{"Id":"abc","State":{"Running":true,"Status":"running"},"Config":{"Labels":{"wsm.tag":"8.10"}},"Other":1}`)
		default:
			w.WriteHeader(http.StatusBadRequest)
			io.WriteString(w, `{"message":"unexpected"}`)
		}
	}))
	ctx := context.Background()
	if err := c.ContainerStart(ctx, "up"); err != nil {
		t.Errorf("start of a running container: %v", err)
	}
	if err := c.ContainerStart(ctx, "down"); err != nil {
		t.Errorf("start: %v", err)
	}
	if err := c.ContainerStart(ctx, "bad"); err == nil || !strings.Contains(err.Error(), "cannot start") {
		t.Errorf("failed start: %v", err)
	}
	if err := c.ContainerRemove(ctx, "up"); err != nil {
		t.Errorf("remove: %v", err)
	}
	st, err := c.ContainerInspect(ctx, "up")
	if err != nil || st.ID != "abc" || !st.Running || st.Labels["wsm.tag"] != "8.10" {
		t.Errorf("inspect = %+v, %v", st, err)
	}
	if !slices.Contains(reqs, "DELETE /containers/up force=true") {
		t.Errorf("requests %v lack a forced remove", reqs)
	}
}

func TestContainerSpecReadOnly(t *testing.T) {
	b, err := json.Marshal(ContainerSpec{Name: "n", Image: "i", ReadOnlyFilesystem: true, ImageVolumeMode: "tmpfs"})
	if err != nil {
		t.Fatal(err)
	}
	if want := `{"name":"n","image":"i","read_only_filesystem":true,"image_volume_mode":"tmpfs"}`; string(b) != want {
		t.Errorf("spec = %s, want %s", b, want)
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

func TestImagePull(t *testing.T) {
	tests := []struct {
		name    string
		policy  string
		stream  string
		status  int
		wantErr string
	}{
		{"ok", PullMissing, `{"stream":"pulling"}` + "\n" + `{"id":"abc","images":["abc"]}`, 200, ""},
		{"always", PullAlways, `{"stream":"pulling"}`, 200, ""},
		{"error object", PullMissing, `{"stream":"pulling"}{"error":"manifest unknown"}`, 200, "manifest unknown"},
		{"garbage", PullMissing, `{"stream":`, 200, "read output"},
		{"http error", PullMissing, `{"message":"denied"}`, 500, "denied"},
		{"too large", PullMissing, `{"stream":"` + strings.Repeat("a", MaxImageOutput) + `"}`, 200, "exceeds"},
		{"bad policy", "newer", ``, 200, "unknown policy"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := fakeServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				q := r.URL.Query()
				if r.Method != http.MethodPost || r.URL.Path != "/v5.0.0/libpod/images/pull" ||
					q.Get("reference") != "docker.io/library/x:1" || q.Get("policy") != tt.policy {
					http.Error(w, `{"message":"bad request"}`, 400)
					return
				}
				w.WriteHeader(tt.status)
				io.WriteString(w, tt.stream)
			}))
			err := c.ImagePull(context.Background(), "docker.io/library/x:1", tt.policy)
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

func TestImageBuild(t *testing.T) {
	const (
		tag  = "localhost/wsm-abcdefghijkl:0123456789ab"
		cf   = "FROM docker.io/library/debian:trixie-slim\nRUN true\n"
		id   = "4f3b0c6a2d1e9f8a7b6c5d4e3f2a1b0c9d8e7f6a5b4c3d2e1f0a9b8c7d6e5f4a"
		good = `{"stream":"STEP 1/2: FROM docker.io/library/debian:trixie-slim\n"}` + "\n" +
			`{"stream":"STEP 2/2: RUN true\n"}` + "\n" +
			`{"stream":"COMMIT localhost/wsm-abcdefghijkl:0123456789ab\n"}` + "\n" +
			`{"stream":"` + id + `\n"}` + "\n"
	)
	tests := []struct {
		name      string
		status    int
		stream    string
		inspectID string
		wantErr   []string
	}{
		{"good stream", 200, good, "sha256:" + id, nil},
		{"error message", 200, `{"stream":"STEP 2/2: RUN false\n"}` + "\n" +
			`{"error":"building at STEP \"RUN false\": exit status 1","errorDetail":{"message":"building at STEP \"RUN false\": exit status 1"}}`,
			id, []string{"exit status 1", "output:\nSTEP 2/2: RUN false"}},
		{"error detail only", 200, `{"errorDetail":{"message":"no space left"}}`, id, []string{"no space left"}},
		{"control characters in output", 200, `{"stream":"\u001b[2Jhi\n"}{"error":"bad\u001b]0;x\u0007"}`, id, []string{"?[2Jhi", "bad?]0;x?"}},
		{"too large", 200, `{"stream":"` + strings.Repeat("a", MaxImageOutput) + `"}`, id, []string{"exceeds"}},
		{"too many messages", 200, strings.Repeat(`{"stream":"aaaaaaaaaaaaaaaa"}`, MaxImageOutput/29+1), id, []string{"exceeds"}},
		{"garbage", 200, `{"stream":"x"}` + "\n" + `<html>`, id, []string{"read output"}},
		{"wrong type", 200, `{"stream":5}`, id, []string{"read output"}},
		{"http error", 500, `{"message":"invalid tag"}`, id, []string{"status 500", "invalid tag"}},
		{"invalid image id", 200, good, "../../etc", []string{"invalid id"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var mu sync.Mutex
			var files []string
			var contentType string
			var query url.Values
			c := fakeServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case r.Method == http.MethodPost && r.URL.Path == "/v5.0.0/libpod/build":
					mu.Lock()
					contentType, query = r.Header.Get("Content-Type"), r.URL.Query()
					tr := tar.NewReader(r.Body)
					for {
						h, err := tr.Next()
						if err != nil {
							break
						}
						data, _ := io.ReadAll(tr)
						files = append(files, fmt.Sprintf("%s %o %d %q", h.Name, h.Mode, h.Typeflag, data))
					}
					mu.Unlock()
					w.WriteHeader(tt.status)
					io.WriteString(w, tt.stream)
				case r.Method == http.MethodGet && r.URL.Path == "/v5.0.0/libpod/images/"+tag+"/json":
					fmt.Fprintf(w, `{"Id":%q}`, tt.inspectID)
				default:
					http.Error(w, `{"message":"unexpected"}`, 400)
				}
			}))
			got, err := c.ImageBuild(context.Background(), tag, cf)
			mu.Lock()
			defer mu.Unlock()
			// The request carries only the Containerfile, whatever the outcome.
			if want := fmt.Sprintf("Containerfile 644 %d %q", tar.TypeReg, cf); len(files) != 1 || files[0] != want {
				t.Errorf("build context = %q, want [%q]", files, want)
			}
			if contentType != "application/x-tar" {
				t.Errorf("Content-Type = %q", contentType)
			}
			wantQuery := url.Values{"t": {tag}, "dockerfile": {"Containerfile"}, "rm": {"true"}}
			if query.Encode() != wantQuery.Encode() {
				t.Errorf("query = %v, want %v", query, wantQuery)
			}
			if tt.wantErr == nil {
				if err != nil || got != id {
					t.Fatalf("ImageBuild = %q, %v; want %q", got, err, id)
				}
				return
			}
			if err == nil {
				t.Fatalf("ImageBuild = %q, want an error", got)
			}
			for _, w := range tt.wantErr {
				if !strings.Contains(err.Error(), w) {
					t.Errorf("err = %q, want it to contain %q", err, w)
				}
			}
			if strings.ContainsAny(err.Error(), "\x1b\x07") {
				t.Errorf("err contains control characters: %q", err)
			}
		})
	}
}

// TestImageBuildErrorTail checks that an error quotes only a bounded tail
// of the build output.
func TestImageBuildErrorTail(t *testing.T) {
	c := fakeServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		for i := range 10000 {
			fmt.Fprintf(w, `{"stream":"line %d\n"}`, i)
		}
		io.WriteString(w, `{"error":"`+strings.Repeat("e", 4*maxErrorMessage)+`"}`)
	}))
	_, err := c.ImageBuild(context.Background(), "localhost/x:1", "FROM x\n")
	if err == nil {
		t.Fatal("no error")
	}
	if n := len(err.Error()); n > maxOutputTail+maxErrorMessage+200 {
		t.Errorf("error is %d bytes", n)
	}
	if !strings.Contains(err.Error(), "line 9999") || strings.Contains(err.Error(), "line 1\n") {
		t.Errorf("error does not end with the tail of the output: %.200q", err)
	}
}
