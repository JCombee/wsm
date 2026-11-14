package sandbox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/JCombee/wsm/internal/db"
	"github.com/JCombee/wsm/internal/podman"
	"github.com/JCombee/wsm/internal/podman/podmantest"
)

type fakeUsers struct {
	mu      sync.Mutex
	calls   []string
	failNew bool
}

func (f *fakeUsers) CreateProjectUser(_ context.Context, uid int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, fmt.Sprintf("create %d", uid))
	if f.failNew {
		return errors.New("useradd failed")
	}
	return nil
}

func (f *fakeUsers) DeleteProjectUser(_ context.Context, uid int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, fmt.Sprintf("delete %d", uid))
	return nil
}

// fakeLibpod is a minimal stateful libpod API. It has no pull endpoint:
// wsm must never pull.
type fakeLibpod struct {
	mu              sync.Mutex
	requests        []string
	bodies          map[string]map[string]any
	pods            map[string]bool
	volumes         map[string]bool
	hasImage        bool
	imageCmd        []string
	imageEntrypoint []string
	failCreate      bool
}

func newFakeLibpod() *fakeLibpod {
	return &fakeLibpod{
		bodies: map[string]map[string]any{}, pods: map[string]bool{}, volumes: map[string]bool{},
		hasImage: true,
	}
}

func (f *fakeLibpod) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	path := strings.TrimPrefix(r.URL.Path, "/v5.0.0/libpod")
	f.requests = append(f.requests, r.Method+" "+path)
	var body map[string]any
	if r.Body != nil {
		data, _ := io.ReadAll(r.Body)
		if len(data) > 0 {
			json.Unmarshal(data, &body)
		}
	}
	notFound := func() {
		w.WriteHeader(http.StatusNotFound)
		io.WriteString(w, `{"message":"no such object","response":404}`)
	}
	// Names are path-escaped, as Podman's own bindings do.
	parts := strings.Split(strings.Trim(strings.TrimPrefix(r.URL.EscapedPath(), "/v5.0.0/libpod"), "/"), "/")
	for i := range parts {
		parts[i], _ = url.PathUnescape(parts[i])
	}
	switch {
	case len(parts) == 3 && parts[0] == "images" && parts[2] == "exists":
		if !f.hasImage {
			notFound()
		} else {
			w.WriteHeader(http.StatusNoContent)
		}
	case len(parts) == 3 && parts[0] == "images" && parts[2] == "json":
		cmd, _ := json.Marshal(f.imageCmd)
		ep, _ := json.Marshal(f.imageEntrypoint)
		fmt.Fprintf(w, `{"Id":"img","Config":{"Cmd":%s,"Entrypoint":%s}}`, cmd, ep)
	case path == "/volumes/create":
		f.bodies["volume "+body["Name"].(string)] = body
		f.volumes[body["Name"].(string)] = true
		w.WriteHeader(http.StatusCreated)
		fmt.Fprintf(w, `{"Name":%q}`, body["Name"])
	case len(parts) == 3 && parts[0] == "volumes" && parts[2] == "exists":
		if f.volumes[parts[1]] {
			w.WriteHeader(http.StatusNoContent)
		} else {
			notFound()
		}
	case len(parts) == 2 && parts[0] == "volumes" && r.Method == http.MethodDelete:
		if !f.volumes[parts[1]] {
			notFound()
			return
		}
		delete(f.volumes, parts[1])
		w.WriteHeader(http.StatusNoContent)
	case path == "/pods/create":
		f.bodies["pod"] = body
		f.pods[body["name"].(string)] = true
		w.WriteHeader(http.StatusCreated)
		io.WriteString(w, `{"Id":"podid"}`)
	case path == "/containers/create":
		f.bodies["container"] = body
		if f.failCreate {
			w.WriteHeader(http.StatusInternalServerError)
			io.WriteString(w, `{"message":"image has no tmux"}`)
			return
		}
		w.WriteHeader(http.StatusCreated)
		io.WriteString(w, `{"Id":"ctrid","Warnings":[]}`)
	case len(parts) == 3 && parts[0] == "pods" && (parts[2] == "start" || parts[2] == "stop"):
		if !f.pods[parts[1]] {
			notFound()
			return
		}
		io.WriteString(w, `{"Errs":[],"Id":"podid"}`)
	case len(parts) == 2 && parts[0] == "pods" && r.Method == http.MethodDelete:
		if !f.pods[parts[1]] {
			notFound()
			return
		}
		delete(f.pods, parts[1])
		io.WriteString(w, `[{"Id":"podid"}]`)
	default:
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprintf(w, `{"message":"fake libpod: unexpected %s %s"}`, r.Method, path)
	}
}

func newManager(t *testing.T) (*Manager, *fakeUsers, *fakeLibpod) {
	t.Helper()
	ctx := context.Background()
	sqlDB, err := db.Open(ctx, filepath.Join(t.TempDir(), "wsm.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { sqlDB.Close() })
	lp := newFakeLibpod()
	srv := podmantest.New(t, lp)
	users := &fakeUsers{}
	m := &Manager{
		Store:  db.NewStore(sqlDB),
		Users:  users,
		Podman: func(int) *podman.Client { return srv.Client() },
		Logf:   t.Logf,
	}
	return m, users, lp
}

func userName(uid int) string { return fmt.Sprintf("wsm-p-%d", uid) }

func TestProjectSpecValidation(t *testing.T) {
	m, users, _ := newManager(t)
	tests := []struct {
		name string
		spec ProjectSpec
	}{
		{"empty slug", ProjectSpec{ImageRef: "img"}},
		{"uppercase slug", ProjectSpec{Slug: "App", ImageRef: "img"}},
		{"slug with slash", ProjectSpec{Slug: "a/b", ImageRef: "img"}},
		{"slug with dots", ProjectSpec{Slug: "..", ImageRef: "img"}},
		{"slug ends in dash", ProjectSpec{Slug: "app-", ImageRef: "img"}},
		{"slug too long", ProjectSpec{Slug: strings.Repeat("a", 33), ImageRef: "img"}},
		{"no image", ProjectSpec{Slug: "app"}},
		{"image with space", ProjectSpec{Slug: "app", ImageRef: "img --cap-add=ALL"}},
		{"image as option", ProjectSpec{Slug: "app", ImageRef: "-v"}},
		{"cpu too low", ProjectSpec{Slug: "app", ImageRef: "img", CPUMillis: 1}},
		{"memory too high", ProjectSpec{Slug: "app", ImageRef: "img", MemBytes: 1 << 50}},
		{"pids negative", ProjectSpec{Slug: "app", ImageRef: "img", PidsLimit: -1}},
		{"control character in name", ProjectSpec{Slug: "app", Name: "a\x1b[2J", ImageRef: "img"}},
		{"image and containerfile", ProjectSpec{Slug: "app", ImageRef: "img", Containerfile: "FROM img\n"}},
		{"blank containerfile", ProjectSpec{Slug: "app", Containerfile: "\n"}},
		{"containerfile with NUL", ProjectSpec{Slug: "app", Containerfile: "FROM img\x00\n"}},
		{"containerfile not UTF-8", ProjectSpec{Slug: "app", Containerfile: "FROM img\xfe\n"}},
		{"containerfile too large", ProjectSpec{Slug: "app", Containerfile: strings.Repeat("#", db.MaxContainerfile+1)}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := m.CreateProject(context.Background(), tt.spec, userName)
			if !errors.Is(err, ErrInvalid) {
				t.Fatalf("err = %v, want ErrInvalid", err)
			}
		})
	}
	if len(users.calls) != 0 {
		t.Errorf("users created for invalid specs: %v", users.calls)
	}
}

func TestSandboxLifecycle(t *testing.T) {
	ctx := context.Background()
	m, users, lp := newManager(t)

	p, err := m.CreateProject(ctx, ProjectSpec{Slug: "app", ImageRef: "localhost/wsm-test:latest",
		CPUMillis: 1500, MemBytes: 512 << 20, PidsLimit: 256}, userName)
	if err != nil {
		t.Fatal(err)
	}
	if p.LinuxUID != 50000 || p.LinuxUser != "wsm-p-50000" {
		t.Fatalf("project user %s/%d", p.LinuxUser, p.LinuxUID)
	}

	sb, err := m.CreateSandbox(ctx, "app", "main")
	if err != nil {
		t.Fatal(err)
	}

	pod := lp.bodies["pod"]
	if pod["name"] != "sbx-"+sb.ID {
		t.Errorf("pod name %v", pod["name"])
	}
	if u, _ := pod["userns"].(map[string]any); u["nsmode"] != "keep-id" || u["value"] != "uid=1000,gid=1000" {
		t.Errorf("pod userns %v", pod["userns"])
	}
	if n, _ := pod["netns"].(map[string]any); n["nsmode"] != "pasta" {
		t.Errorf("pod netns %v", pod["netns"])
	}

	ctr := lp.bodies["container"]
	checks := map[string]any{
		"name":              "sbx-" + sb.ID + "-work",
		"pod":               "sbx-" + sb.ID,
		"user":              "1000:1000",
		"init":              true,
		"no_new_privileges": true,
		"work_dir":          "/workspace",
	}
	for k, want := range checks {
		if ctr[k] != want {
			t.Errorf("container %s = %v, want %v", k, ctr[k], want)
		}
	}
	if fmt.Sprint(ctr["cap_drop"]) != "[ALL]" {
		t.Errorf("cap_drop = %v", ctr["cap_drop"])
	}
	if fmt.Sprint(ctr["command"]) != "[sleep infinity]" {
		t.Errorf("command = %v", ctr["command"])
	}
	env, _ := ctr["env"].(map[string]any)
	if env["WSM_PROJECT"] != "app" || env["WSM_SANDBOX"] != "main" {
		t.Errorf("env = %v", env)
	}
	res, _ := json.Marshal(ctr["resource_limits"])
	if want := `{"cpu":{"period":100000,"quota":150000},"memory":{"limit":536870912},"pids":{"limit":256}}`; string(res) != want {
		t.Errorf("resource_limits = %s, want %s", res, want)
	}
	vols, _ := json.Marshal(ctr["volumes"])
	wantVols := fmt.Sprintf(`[{"Dest":"/workspace","Name":"sbx-%s-code"},{"Dest":"/home/agent","Name":"proj-%s-home"}]`, sb.ID, p.ID)
	if string(vols) != wantVols {
		t.Errorf("volumes = %s, want %s", vols, wantVols)
	}
	for _, k := range []string{"privileged", "mounts", "devices", "netns", "userns", "seccomp_profile_path", "cap_add"} {
		if _, ok := ctr[k]; ok {
			t.Errorf("container spec sets %q", k)
		}
	}

	if _, _, err := m.ShellTarget(ctx, "app", "main"); !errors.Is(err, ErrState) {
		t.Errorf("shell on a created sandbox: %v", err)
	}
	if err := m.StartSandbox(ctx, "app", "main"); err != nil {
		t.Fatal(err)
	}
	pc, ctrName, err := m.ShellTarget(ctx, "app", "main")
	if err != nil || ctrName != "sbx-"+sb.ID+"-work" {
		t.Fatalf("ShellTarget = %q, %v", ctrName, err)
	}
	pc.Close()
	if err := m.StopSandbox(ctx, "app", "main"); err != nil {
		t.Fatal(err)
	}
	list, err := m.Sandboxes(ctx, "app")
	if err != nil || len(list) != 1 || list[0].Status != db.StatusStopped {
		t.Fatalf("Sandboxes = %+v, %v", list, err)
	}

	if err := m.DeleteSandbox(ctx, "app", "main"); err != nil {
		t.Fatal(err)
	}
	if lp.pods["sbx-"+sb.ID] || lp.volumes["sbx-"+sb.ID+"-code"] {
		t.Error("pod or code volume left after DeleteSandbox")
	}
	if !lp.volumes["proj-"+p.ID+"-home"] {
		t.Error("project home volume removed with the sandbox")
	}

	if _, err := m.CreateSandbox(ctx, "app", "second"); err != nil {
		t.Fatal(err)
	}
	if err := m.DeleteProject(ctx, "app"); err != nil {
		t.Fatal(err)
	}
	if len(lp.pods) != 0 || len(lp.volumes) != 0 {
		t.Errorf("left after DeleteProject: pods %v volumes %v", lp.pods, lp.volumes)
	}
	if got := strings.Join(users.calls, ","); got != "create 50000,delete 50000" {
		t.Errorf("user calls = %s", got)
	}
	if ps, _ := m.Projects(ctx); len(ps) != 0 {
		t.Errorf("projects left: %+v", ps)
	}
	if sbs, _ := m.Sandboxes(ctx, ""); len(sbs) != 0 {
		t.Errorf("sandboxes left: %+v", sbs)
	}
}

// TestImageCommandIsKept checks that an image's own CMD or ENTRYPOINT runs;
// only an image with neither gets "sleep infinity" (ADR 0007).
func TestImageCommandIsKept(t *testing.T) {
	tests := []struct {
		name            string
		cmd, entrypoint []string
	}{
		{"cmd", []string{"supervisord"}, nil},
		{"entrypoint", nil, []string{"/entrypoint.sh"}},
		{"both", []string{"--serve"}, []string{"/entrypoint.sh"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			m, _, lp := newManager(t)
			lp.imageCmd, lp.imageEntrypoint = tt.cmd, tt.entrypoint
			if _, err := m.CreateProject(ctx, ProjectSpec{Slug: "app", ImageRef: "img"}, userName); err != nil {
				t.Fatal(err)
			}
			if _, err := m.CreateSandbox(ctx, "app", "main"); err != nil {
				t.Fatal(err)
			}
			if _, ok := lp.bodies["container"]["command"]; ok {
				t.Errorf("command overridden: %v", lp.bodies["container"]["command"])
			}
		})
	}
}

func TestCreateSandboxNeedsLocalImage(t *testing.T) {
	ctx := context.Background()
	m, _, lp := newManager(t)
	lp.hasImage = false
	if _, err := m.CreateProject(ctx, ProjectSpec{Slug: "app", ImageRef: "docker.io/library/x:1"}, userName); err != nil {
		t.Fatal(err)
	}
	_, err := m.CreateSandbox(ctx, "app", "main")
	if !errors.Is(err, ErrState) || !strings.Contains(err.Error(), "docker.io/library/x:1 is not present") {
		t.Fatalf("err = %v, want ErrState for the missing image", err)
	}
	for _, r := range lp.requests {
		if strings.Contains(r, "/pull") {
			t.Errorf("wsm tried to pull: %s", r)
		}
	}
	if len(lp.pods) != 0 || len(lp.volumes) != 0 {
		t.Errorf("left: pods %v volumes %v", lp.pods, lp.volumes)
	}
	if sbs, _ := m.Sandboxes(ctx, "app"); len(sbs) != 0 {
		t.Errorf("sandbox rows left: %+v", sbs)
	}
}

func TestCreateSandboxRollsBack(t *testing.T) {
	ctx := context.Background()
	m, _, lp := newManager(t)
	if _, err := m.CreateProject(ctx, ProjectSpec{Slug: "app", ImageRef: "img"}, userName); err != nil {
		t.Fatal(err)
	}
	lp.failCreate = true
	if _, err := m.CreateSandbox(ctx, "app", "main"); err == nil || !strings.Contains(err.Error(), "image has no tmux") {
		t.Fatalf("err = %v", err)
	}
	if len(lp.pods) != 0 {
		t.Errorf("pods left: %v", lp.pods)
	}
	for v := range lp.volumes {
		if strings.HasSuffix(v, "-code") {
			t.Errorf("code volume left: %s", v)
		}
	}
	if sbs, _ := m.Sandboxes(ctx, "app"); len(sbs) != 0 {
		t.Errorf("sandbox rows left: %+v", sbs)
	}
}

func TestCreateProjectRollsBack(t *testing.T) {
	ctx := context.Background()
	m, users, _ := newManager(t)
	users.failNew = true
	if _, err := m.CreateProject(ctx, ProjectSpec{Slug: "app", ImageRef: "img"}, userName); err == nil {
		t.Fatal("CreateProject succeeded")
	}
	if got := strings.Join(users.calls, ","); got != "create 50000,delete 50000" {
		t.Errorf("user calls = %s", got)
	}
	if ps, _ := m.Projects(ctx); len(ps) != 0 {
		t.Errorf("project left: %+v", ps)
	}
}
