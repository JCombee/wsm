package sandbox

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/JCombee/wsm/internal/blocks"
	"github.com/JCombee/wsm/internal/db"
	"github.com/JCombee/wsm/internal/podman"
	"github.com/JCombee/wsm/internal/podman/podmantest"
)

func redisBlock(t *testing.T) blocks.Block {
	t.Helper()
	b, ok := blocks.Lookup("redis")
	if !ok {
		t.Fatal("redis is missing from the catalog")
	}
	return b
}

func TestRedisSpec(t *testing.T) {
	p := db.Project{ID: "p1", Slug: "app"}
	sb := db.Sandbox{ID: "s1", Slug: "main"}
	got, err := ServiceSpec(p, sb, redisBlock(t), "8.10")
	if err != nil {
		t.Fatal(err)
	}
	want := podman.ContainerSpec{
		Name:  "sbx-s1-redis",
		Image: "docker.io/library/redis:8.10",
		Pod:   "sbx-s1",
		Labels: map[string]string{"wsm.project": "p1", "wsm.sandbox": "s1",
			"wsm.service": "redis", "wsm.tag": "8.10"},
		User: "1000:1000",
		Command: []string{"redis-server", "--bind", "127.0.0.1", "--port", "6379",
			"--save", "", "--appendonly", "no", "--maxmemory", "200mb", "--maxmemory-policy", "noeviction"},
		CapDrop:            []string{"ALL"},
		NoNewPrivileges:    true,
		ReadOnlyFilesystem: true,
		ImageVolumeMode:    "tmpfs",
		Resources: &podman.Resources{
			CPU:    &podman.CPULimit{Quota: 50000, Period: 100000},
			Memory: &podman.MemoryLimit{Limit: 268435456},
			Pids:   &podman.PidsLimit{Limit: 64},
		},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("spec =\n%+v\nwant\n%+v", got, want)
	}
	// The JSON body has exactly these keys: no volumes, ports, host paths,
	// env or init.
	b, _ := json.Marshal(got)
	var body map[string]any
	json.Unmarshal(b, &body)
	var keys []string
	for k := range body {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	wantKeys := []string{"cap_drop", "command", "image", "image_volume_mode", "labels", "name",
		"no_new_privileges", "pod", "read_only_filesystem", "resource_limits", "user"}
	if !reflect.DeepEqual(keys, wantKeys) {
		t.Errorf("body keys %v, want %v: %s", keys, wantKeys, b)
	}
	if _, err := ServiceSpec(p, sb, redisBlock(t), "7.1"); err == nil {
		t.Error("ServiceSpec accepted a tag below the minimum")
	}
	maria, _ := blocks.Lookup("mariadb")
	if _, err := ServiceSpec(p, sb, maria, "11.8"); err == nil {
		t.Error("ServiceSpec accepted a shared block")
	}
}

// fakeServices is a libpod API with service containers, image pulls and an
// exec whose stdout is set by the test.
type fakeServices struct {
	mu         sync.Mutex
	requests   []string
	containers map[string]podman.ContainerSpec // by name
	running    map[string]bool
	ids        map[string]string
	nextID     int
	pulls      []string
	// probeOut is what every exec prints on stdout; probeCode its exit code.
	probeOut  string
	probeCode int
	unknown   map[string]bool // image refs that do not exist
}

func newFakeServices() *fakeServices {
	return &fakeServices{containers: map[string]podman.ContainerSpec{}, running: map[string]bool{},
		ids: map[string]string{}, probeOut: "PONG\n", unknown: map[string]bool{}}
}

func (f *fakeServices) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	path := strings.TrimPrefix(r.URL.Path, "/v5.0.0/libpod")
	parts := strings.Split(strings.Trim(strings.TrimPrefix(r.URL.EscapedPath(), "/v5.0.0/libpod"), "/"), "/")
	for i := range parts {
		parts[i], _ = url.PathUnescape(parts[i])
	}
	if !(len(parts) == 3 && parts[0] == "exec") {
		f.requests = append(f.requests, r.Method+" "+path)
	}
	notFound := func() {
		w.WriteHeader(http.StatusNotFound)
		io.WriteString(w, `{"message":"no such container","response":404}`)
	}
	switch {
	case path == "/images/pull":
		ref := r.URL.Query().Get("reference")
		f.pulls = append(f.pulls, ref+" "+r.URL.Query().Get("policy"))
		if f.unknown[ref] {
			fmt.Fprintf(w, `{"error":"initializing source docker://%s: reading manifest: manifest unknown"}`, ref)
			return
		}
		io.WriteString(w, `{"stream":"ok"}`)
	case path == "/containers/create":
		var spec podman.ContainerSpec
		json.NewDecoder(r.Body).Decode(&spec)
		if _, ok := f.containers[spec.Name]; ok {
			w.WriteHeader(http.StatusInternalServerError)
			io.WriteString(w, `{"message":"name in use"}`)
			return
		}
		f.nextID++
		f.containers[spec.Name] = spec
		f.ids[spec.Name] = fmt.Sprintf("ctr%d", f.nextID)
		w.WriteHeader(http.StatusCreated)
		fmt.Fprintf(w, `{"Id":%q}`, f.ids[spec.Name])
	case len(parts) == 3 && parts[0] == "containers" && parts[2] == "json":
		spec, ok := f.containers[parts[1]]
		if !ok {
			notFound()
			return
		}
		labels, _ := json.Marshal(spec.Labels)
		fmt.Fprintf(w, `{"Id":%q,"State":{"Running":%v},"Config":{"Labels":%s}}`, f.ids[parts[1]], f.running[parts[1]], labels)
	case len(parts) == 3 && parts[0] == "containers" && parts[2] == "start":
		if _, ok := f.containers[parts[1]]; !ok {
			notFound()
			return
		}
		if f.running[parts[1]] {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		f.running[parts[1]] = true
		w.WriteHeader(http.StatusNoContent)
	case len(parts) == 2 && parts[0] == "containers" && r.Method == http.MethodDelete:
		if _, ok := f.containers[parts[1]]; !ok {
			notFound()
			return
		}
		delete(f.containers, parts[1])
		delete(f.running, parts[1])
		io.WriteString(w, `[{"Id":"x"}]`)
	case len(parts) == 3 && parts[0] == "containers" && parts[2] == "exec":
		w.WriteHeader(http.StatusCreated)
		io.WriteString(w, `{"Id":"exec1"}`)
	case len(parts) == 3 && parts[0] == "exec" && parts[2] == "start":
		conn, _, err := w.(http.Hijacker).Hijack()
		if err != nil {
			return
		}
		defer conn.Close()
		io.WriteString(conn, "HTTP/1.1 101 UPGRADED\r\nContent-Type: application/vnd.docker.raw-stream\r\nConnection: Upgrade\r\nUpgrade: tcp\r\n\r\n")
		if f.probeOut != "" {
			hdr := make([]byte, 8)
			hdr[0] = 1
			binary.BigEndian.PutUint32(hdr[4:], uint32(len(f.probeOut)))
			conn.Write(append(hdr, f.probeOut...))
		}
	case len(parts) == 3 && parts[0] == "exec" && parts[2] == "json":
		fmt.Fprintf(w, `{"Running":false,"ExitCode":%d}`, f.probeCode)
	default:
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprintf(w, `{"message":"fake: unexpected %s %s"}`, r.Method, path)
	}
}

func (f *fakeServices) take() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	r := f.requests
	f.requests = nil
	return r
}

// serviceFixture is a manager with one project and one running sandbox.
type serviceFixture struct {
	m  *Manager
	f  *fakeServices
	p  db.Project
	sb db.Sandbox
}

func newServiceFixture(t *testing.T) *serviceFixture {
	t.Helper()
	ctx := context.Background()
	sqlDB, err := db.Open(ctx, filepath.Join(t.TempDir(), "wsm.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { sqlDB.Close() })
	f := newFakeServices()
	srv := podmantest.New(t, f)
	m := &Manager{Store: db.NewStore(sqlDB), Podman: func(int) *podman.Client { return srv.Client() }, Logf: t.Logf}
	p := db.Project{Slug: "app", Name: "app", ImageRef: "localhost/x:1", CPUMillis: 1000, MemBytes: 1 << 30, PidsLimit: 256}
	if err := m.Store.CreateProject(ctx, &p, userName); err != nil {
		t.Fatal(err)
	}
	sb := db.Sandbox{ProjectID: p.ID, Slug: "main", MCPTokenHash: strings.Repeat("a", 64)}
	if err := m.Store.CreateSandbox(ctx, &sb, PodName); err != nil {
		t.Fatal(err)
	}
	if err := m.Store.SetSandboxStatus(ctx, sb.ID, db.StatusRunning); err != nil {
		t.Fatal(err)
	}
	return &serviceFixture{m: m, f: f, p: p, sb: sb}
}

func TestEnsureService(t *testing.T) {
	redis := redisBlock(t)
	ctxT := func(t *testing.T, d time.Duration) context.Context {
		ctx, cancel := context.WithTimeout(context.Background(), d)
		t.Cleanup(cancel)
		return ctx
	}
	recorder := func(calls *[]string, f *fakeServices, err error) func(context.Context) error {
		return func(context.Context) error {
			f.mu.Lock()
			n := len(f.requests)
			f.mu.Unlock()
			*calls = append(*calls, fmt.Sprintf("record after %d requests", n))
			return err
		}
	}

	t.Run("create, start and wait", func(t *testing.T) {
		x := newServiceFixture(t)
		var rec []string
		if err := x.m.EnsureService(ctxT(t, 10*time.Second), x.sb.ID, redis, "8.10", recorder(&rec, x.f, nil)); err != nil {
			t.Fatal(err)
		}
		name := "sbx-" + x.sb.ID + "-redis"
		want := []string{
			"GET /containers/" + name + "/json",
			"POST /images/pull",
			"POST /containers/create",
			"POST /containers/" + name + "/start",
			"POST /containers/" + name + "/exec",
		}
		if got := x.f.take(); !reflect.DeepEqual(got, want) {
			t.Errorf("requests = %v\nwant %v", got, want)
		}
		if !reflect.DeepEqual(rec, []string{"record after 0 requests"}) {
			t.Errorf("record = %v, want it before any Podman call", rec)
		}
		if !reflect.DeepEqual(x.f.pulls, []string{"docker.io/library/redis:8.10 missing"}) {
			t.Errorf("pulls = %v", x.f.pulls)
		}
		if x.f.containers[name].Labels["wsm.tag"] != "8.10" || !x.f.running[name] {
			t.Errorf("container %+v", x.f.containers[name])
		}

		// Same tag again: no new container.
		if err := x.m.EnsureService(ctxT(t, 10*time.Second), x.sb.ID, redis, "8.10", recorder(&rec, x.f, nil)); err != nil {
			t.Fatal(err)
		}
		want = []string{
			"GET /containers/" + name + "/json",
			"POST /containers/" + name + "/start",
			"POST /containers/" + name + "/exec",
		}
		if got := x.f.take(); !reflect.DeepEqual(got, want) {
			t.Errorf("same tag: requests = %v\nwant %v", got, want)
		}
		id := x.f.ids[name]

		// Another tag: removed first, then created again.
		if err := x.m.EnsureService(ctxT(t, 10*time.Second), x.sb.ID, redis, "8.8", recorder(&rec, x.f, nil)); err != nil {
			t.Fatal(err)
		}
		want = []string{
			"GET /containers/" + name + "/json",
			"DELETE /containers/" + name,
			"POST /images/pull",
			"POST /containers/create",
			"POST /containers/" + name + "/start",
			"POST /containers/" + name + "/exec",
		}
		if got := x.f.take(); !reflect.DeepEqual(got, want) {
			t.Errorf("other tag: requests = %v\nwant %v", got, want)
		}
		if x.f.ids[name] == id || x.f.containers[name].Image != "docker.io/library/redis:8.8" {
			t.Errorf("container was not replaced: %s %+v", x.f.ids[name], x.f.containers[name])
		}
	})

	t.Run("record error leaves the container alone", func(t *testing.T) {
		x := newServiceFixture(t)
		boom := errors.New("ceiling changed")
		var rec []string
		err := x.m.EnsureService(ctxT(t, 10*time.Second), x.sb.ID, redis, "8.10", recorder(&rec, x.f, boom))
		if err != boom {
			t.Errorf("err = %v, want the record error unchanged", err)
		}
		if got := x.f.take(); len(got) != 0 {
			t.Errorf("requests after a failed record: %v", got)
		}
	})

	t.Run("deleted sandbox is denied", func(t *testing.T) {
		x := newServiceFixture(t)
		if err := x.m.Store.ClearMCPToken(context.Background(), x.sb.ID); err != nil {
			t.Fatal(err)
		}
		var rec []string
		err := x.m.EnsureService(ctxT(t, 10*time.Second), x.sb.ID, redis, "8.10", recorder(&rec, x.f, nil))
		if !errors.Is(err, ErrServiceDenied) || len(rec) != 0 || len(x.f.take()) != 0 {
			t.Errorf("err = %v, record = %v", err, rec)
		}
		err = x.m.EnsureService(ctxT(t, 10*time.Second), "nosuchsandbox", redis, "8.10", recorder(&rec, x.f, nil))
		if !errors.Is(err, ErrServiceDenied) {
			t.Errorf("missing sandbox: err = %v", err)
		}
	})

	t.Run("stopped sandbox is unavailable", func(t *testing.T) {
		x := newServiceFixture(t)
		if err := x.m.Store.SetSandboxStatus(context.Background(), x.sb.ID, db.StatusStopped); err != nil {
			t.Fatal(err)
		}
		var rec []string
		err := x.m.EnsureService(ctxT(t, 10*time.Second), x.sb.ID, redis, "8.10", recorder(&rec, x.f, nil))
		if !errors.Is(err, ErrServiceUnavailable) || len(rec) != 0 || len(x.f.take()) != 0 {
			t.Errorf("err = %v, record = %v", err, rec)
		}
	})

	t.Run("no PONG before the deadline", func(t *testing.T) {
		for _, out := range []string{"", "PONG", "PONG\nPONG\n", "pong\n", "+PONG\r\n", "PONG\n\x00"} {
			x := newServiceFixture(t)
			x.f.probeOut = out
			var rec []string
			err := x.m.EnsureService(ctxT(t, 700*time.Millisecond), x.sb.ID, redis, "8.10", recorder(&rec, x.f, nil))
			if !errors.Is(err, ErrServiceUnavailable) {
				t.Errorf("output %q: err = %v, want ErrServiceUnavailable", out, err)
			}
		}
		x := newServiceFixture(t)
		x.f.probeCode = 1
		var rec []string
		err := x.m.EnsureService(ctxT(t, 700*time.Millisecond), x.sb.ID, redis, "8.10", recorder(&rec, x.f, nil))
		if !errors.Is(err, ErrServiceUnavailable) {
			t.Errorf("failing probe: err = %v", err)
		}
	})

	t.Run("busy manager until the deadline", func(t *testing.T) {
		x := newServiceFixture(t)
		x.m.mu.Lock()
		defer x.m.mu.Unlock()
		var rec []string
		start := time.Now()
		err := x.m.EnsureService(ctxT(t, 300*time.Millisecond), x.sb.ID, redis, "8.10", recorder(&rec, x.f, nil))
		if !errors.Is(err, ErrServiceUnavailable) || len(rec) != 0 {
			t.Errorf("err = %v, record = %v", err, rec)
		}
		if d := time.Since(start); d > 2*time.Second {
			t.Errorf("waited %v for the lock", d)
		}
	})
}

func TestPullServiceImage(t *testing.T) {
	x := newServiceFixture(t)
	ctx := context.Background()
	redis := redisBlock(t)
	if err := x.m.PullServiceImage(ctx, x.p.ID, redis, "8.10"); err != nil {
		t.Fatal(err)
	}
	x.f.unknown["docker.io/library/redis:99.99"] = true
	if err := x.m.PullServiceImage(ctx, x.p.ID, redis, "99.99"); !errors.Is(err, ErrUnknownTag) {
		t.Errorf("unknown tag: err = %v", err)
	}
	if err := x.m.PullServiceImage(ctx, x.p.ID, redis, "6.2"); err == nil || errors.Is(err, ErrUnknownTag) {
		t.Errorf("tag below the minimum: err = %v", err)
	}
	if !reflect.DeepEqual(x.f.pulls, []string{"docker.io/library/redis:8.10 missing", "docker.io/library/redis:99.99 missing"}) {
		t.Errorf("pulls = %v", x.f.pulls)
	}
}
