package sandbox

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
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
	"github.com/JCombee/wsm/internal/gitmgr"
	"github.com/JCombee/wsm/internal/podman"
	"github.com/JCombee/wsm/internal/podman/podmantest"
	"github.com/JCombee/wsm/internal/secret"
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

// fakeLibpod is a minimal stateful libpod API.
type fakeLibpod struct {
	mu       sync.Mutex
	requests []string
	bodies   map[string]map[string]any
	pods     map[string]bool
	volumes  map[string]bool
	// anyImage makes every image exist; otherwise only those in images.
	anyImage bool
	images   map[string]bool
	// builds holds the Containerfile of every build, by tag.
	builds          map[string][]string
	pulls           []string
	failPull        bool
	failBuild       bool
	imageCmd        []string
	imageEntrypoint []string
	failCreate      bool
	// execs holds every exec's command, joined by spaces, in order.
	execs []string
	// execStdin holds the stdin of every exec that attached one.
	execStdin map[int][]byte
	// execFail maps the first two words of a command to its exit code
	// and stderr.
	execFail    map[string]execOutcome
	execByID    map[string]execCall
	podsStopped []string
}

type execOutcome struct {
	code   int
	stderr string
}

type execCall struct {
	n     int
	cmd   []string
	stdin bool
}

func newFakeLibpod() *fakeLibpod {
	return &fakeLibpod{
		bodies: map[string]map[string]any{}, pods: map[string]bool{}, volumes: map[string]bool{},
		anyImage: true, images: map[string]bool{}, builds: map[string][]string{},
		execStdin: map[int][]byte{}, execFail: map[string]execOutcome{}, execByID: map[string]execCall{},
	}
}

// fakeImageID is the ID of every image in the fake.
const fakeImageID = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

// readContainerfile returns the Containerfile from a build context, and
// fails if the context holds anything else.
func readContainerfile(data []byte) (string, error) {
	tr := tar.NewReader(bytes.NewReader(data))
	var cf string
	n := 0
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return "", err
		}
		n++
		if h.Name != "Containerfile" || n > 1 {
			return "", fmt.Errorf("unexpected file %q in the build context", h.Name)
		}
		b, err := io.ReadAll(tr)
		if err != nil {
			return "", err
		}
		cf = string(b)
	}
	if n != 1 {
		return "", errors.New("no Containerfile in the build context")
	}
	return cf, nil
}

func (f *fakeLibpod) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	path := strings.TrimPrefix(r.URL.Path, "/v5.0.0/libpod")
	f.requests = append(f.requests, r.Method+" "+path)
	var body map[string]any
	var data []byte
	if r.Body != nil {
		data, _ = io.ReadAll(r.Body)
		if len(data) > 0 && r.Header.Get("Content-Type") == "application/json" {
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
		if !f.anyImage && !f.images[parts[1]] {
			notFound()
		} else {
			w.WriteHeader(http.StatusNoContent)
		}
	case len(parts) == 3 && parts[0] == "images" && parts[2] == "json":
		if !f.anyImage && !f.images[parts[1]] {
			notFound()
			return
		}
		cmd, _ := json.Marshal(f.imageCmd)
		ep, _ := json.Marshal(f.imageEntrypoint)
		fmt.Fprintf(w, `{"Id":"sha256:%s","Config":{"Cmd":%s,"Entrypoint":%s}}`, fakeImageID, cmd, ep)
	case path == "/images/pull" && r.Method == http.MethodPost:
		ref := r.URL.Query().Get("reference")
		f.pulls = append(f.pulls, ref+" "+r.URL.Query().Get("policy"))
		if f.failPull {
			io.WriteString(w, `{"stream":"Trying to pull"}`+"\n"+`{"error":"manifest unknown"}`)
			return
		}
		f.images[ref] = true
		fmt.Fprintf(w, `{"stream":"Trying to pull"}`+"\n"+`{"id":%q,"images":[%q]}`, fakeImageID, fakeImageID)
	case path == "/build" && r.Method == http.MethodPost:
		q := r.URL.Query()
		cf, err := readContainerfile(data)
		if err != nil || q.Get("dockerfile") != "Containerfile" || q.Get("rm") != "true" {
			w.WriteHeader(http.StatusBadRequest)
			fmt.Fprintf(w, `{"message":"fake libpod: bad build: %v %v"}`, err, q)
			return
		}
		tag := q.Get("t")
		f.builds[tag] = append(f.builds[tag], cf)
		if f.failBuild {
			io.WriteString(w, `{"stream":"STEP 1/2: FROM x\n"}`+"\n"+`{"error":"building at STEP \"RUN false\": exit status 1"}`)
			return
		}
		f.images[tag] = true
		fmt.Fprintf(w, `{"stream":"STEP 1/2: FROM x\n"}`+"\n"+`{"stream":"%s\n"}`, fakeImageID)
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
		if parts[2] == "stop" {
			f.podsStopped = append(f.podsStopped, parts[1])
		}
		io.WriteString(w, `{"Errs":[],"Id":"podid"}`)
	case len(parts) == 3 && parts[0] == "containers" && parts[2] == "exec":
		var cfg struct {
			Cmd         []string
			AttachStdin bool
		}
		json.Unmarshal(data, &cfg)
		id := fmt.Sprintf("exec%d", len(f.execs))
		f.execByID[id] = execCall{n: len(f.execs), cmd: cfg.Cmd, stdin: cfg.AttachStdin}
		f.execs = append(f.execs, strings.Join(cfg.Cmd, " "))
		w.WriteHeader(http.StatusCreated)
		fmt.Fprintf(w, `{"Id":%q}`, id)
	case len(parts) == 3 && parts[0] == "exec" && parts[2] == "start":
		call := f.execByID[parts[1]]
		conn, brw, err := w.(http.Hijacker).Hijack()
		if err != nil {
			return
		}
		defer conn.Close()
		io.WriteString(conn, "HTTP/1.1 101 UPGRADED\r\nContent-Type: application/vnd.docker.raw-stream\r\nConnection: Upgrade\r\nUpgrade: tcp\r\n\r\n")
		if call.stdin {
			f.execStdin[call.n], _ = io.ReadAll(brw)
		}
		if o, ok := f.execFail[firstWords(call.cmd)]; ok && o.stderr != "" {
			hdr := make([]byte, 8)
			hdr[0] = 2
			binary.BigEndian.PutUint32(hdr[4:], uint32(len(o.stderr)))
			conn.Write(append(hdr, o.stderr...))
		}
	case len(parts) == 3 && parts[0] == "exec" && parts[2] == "json":
		call := f.execByID[parts[1]]
		fmt.Fprintf(w, `{"Running":false,"ExitCode":%d}`, f.execFail[firstWords(call.cmd)].code)
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

// firstWords returns the first two words of a command.
func firstWords(cmd []string) string {
	if len(cmd) < 2 {
		return strings.Join(cmd, " ")
	}
	return cmd[0] + " " + cmd[1]
}

// fakeMirrors records mirror calls and serves a fixed bundle.
type fakeMirrors struct {
	mu         sync.Mutex
	updates    []gitmgr.Repo
	bundles    []string
	removed    []string
	bundle     []byte
	failUpdate bool
}

func (f *fakeMirrors) Update(_ context.Context, r gitmgr.Repo) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	r.Key = bytes.Clone(r.Key)
	f.updates = append(f.updates, r)
	if f.failUpdate {
		return errors.New("git fetch: exit status 128: Permission denied (publickey)")
	}
	return nil
}

func (f *fakeMirrors) Bundle(_ context.Context, projectID, branch string, w io.Writer) error {
	f.mu.Lock()
	f.bundles = append(f.bundles, projectID+" "+branch)
	b := f.bundle
	f.mu.Unlock()
	_, err := w.Write(b)
	return err
}

func (f *fakeMirrors) Remove(projectID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.removed = append(f.removed, projectID)
	return nil
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
	box, err := secret.New(bytes.Repeat([]byte{7}, secret.KeySize))
	if err != nil {
		t.Fatal(err)
	}
	m := &Manager{
		Store:   db.NewStore(sqlDB),
		Users:   users,
		Podman:  func(int) *podman.Client { return srv.Client() },
		Secrets: box,
		Mirrors: &fakeMirrors{bundle: []byte("# v2 git bundle\nfake bundle data\n")},
		Logf:    t.Logf,
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

// TestCreateSandboxImageUnavailable checks that a registry image that
// cannot be pulled fails the sandbox and leaves nothing behind. M1 failed
// here with "image ... is not present"; since M2a wsm pulls the image.
func TestCreateSandboxImageUnavailable(t *testing.T) {
	ctx := context.Background()
	m, _, lp := newManager(t)
	lp.anyImage, lp.failPull = false, true
	if _, err := m.CreateProject(ctx, ProjectSpec{Slug: "app", ImageRef: "docker.io/library/x:1"}, userName); err != nil {
		t.Fatal(err)
	}
	_, err := m.CreateSandbox(ctx, "app", "main")
	if err == nil || !strings.Contains(err.Error(), "pull docker.io/library/x:1: manifest unknown") {
		t.Fatalf("err = %v, want the pull error", err)
	}
	if len(lp.builds) != 0 {
		t.Errorf("a registry project was built: %v", lp.builds)
	}
	if len(lp.pods) != 0 || len(lp.volumes) != 0 {
		t.Errorf("left: pods %v volumes %v", lp.pods, lp.volumes)
	}
	if sbs, _ := m.Sandboxes(ctx, "app"); len(sbs) != 0 {
		t.Errorf("sandbox rows left: %+v", sbs)
	}
}

func TestProjectImage(t *testing.T) {
	cf := "FROM docker.io/library/debian:trixie-slim\n"
	sum := sha256.Sum256([]byte(cf))
	tests := []struct {
		name string
		p    db.Project
		want string
	}{
		{"registry", db.Project{ID: "abcdefghijkl", ImageSource: db.ImageSourceRegistry, ImageRef: "docker.io/library/debian:trixie-slim"},
			"docker.io/library/debian:trixie-slim"},
		{"containerfile", db.Project{ID: "abcdefghijkl", ImageSource: db.ImageSourceContainerfile, Containerfile: cf},
			"localhost/wsm-abcdefghijkl:" + hex.EncodeToString(sum[:])[:12]},
		{"changed containerfile", db.Project{ID: "abcdefghijkl", ImageSource: db.ImageSourceContainerfile, Containerfile: cf + "RUN true\n"},
			"localhost/wsm-abcdefghijkl:" + func() string { s := sha256.Sum256([]byte(cf + "RUN true\n")); return hex.EncodeToString(s[:])[:12] }()},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ProjectImage(tt.p)
			if got != tt.want {
				t.Errorf("ProjectImage = %q, want %q", got, tt.want)
			}
			if !imagePattern.MatchString(got) {
				t.Errorf("%q does not match the image pattern", got)
			}
		})
	}
}

func TestSandboxEnsuresImage(t *testing.T) {
	const (
		registryRef = "docker.io/library/debian:trixie-slim"
		cf          = "FROM docker.io/library/debian:trixie-slim\nCMD [\"sleep\",\"infinity\"]\n"
	)
	tests := []struct {
		name          string
		containerfile bool
		present       bool
		failPull      bool
		failBuild     bool
		wantPulls     int
		wantBuilds    int
		wantErr       string
	}{
		{"registry image present: no pull", false, true, false, false, 0, 0, ""},
		{"registry image missing: pull", false, false, false, false, 1, 0, ""},
		{"build tag present: no build", true, true, false, false, 0, 0, ""},
		{"build tag missing: build", true, false, false, false, 0, 1, ""},
		{"pull fails: rollback", false, false, true, false, 1, 0, "manifest unknown"},
		{"build fails: rollback", true, false, false, true, 0, 1, "exit status 1"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			m, _, lp := newManager(t)
			lp.anyImage, lp.failPull, lp.failBuild = false, tt.failPull, tt.failBuild
			spec := ProjectSpec{Slug: "app", ImageRef: registryRef}
			if tt.containerfile {
				spec = ProjectSpec{Slug: "app", Containerfile: cf}
			}
			p, err := m.CreateProject(ctx, spec, userName)
			if err != nil {
				t.Fatal(err)
			}
			ref := ProjectImage(p)
			if tt.present {
				lp.images[ref] = true
			}
			if len(lp.pulls) != 0 || len(lp.builds) != 0 {
				t.Fatalf("project create got an image: pulls %v builds %v", lp.pulls, lp.builds)
			}

			sb, err := m.CreateSandbox(ctx, "app", "main")

			if len(lp.pulls) != tt.wantPulls {
				t.Errorf("pulls = %v, want %d", lp.pulls, tt.wantPulls)
			}
			for _, pull := range lp.pulls {
				if pull != registryRef+" missing" {
					t.Errorf("pull = %q, want %q with policy missing", pull, registryRef)
				}
			}
			if n := len(lp.builds[ref]); n != tt.wantBuilds || len(lp.builds) > 1 {
				t.Errorf("builds = %v, want %d of %s", lp.builds, tt.wantBuilds, ref)
			}
			for _, got := range lp.builds[ref] {
				if got != cf {
					t.Errorf("built %q, want the stored Containerfile", got)
				}
			}
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("err = %v, want %q", err, tt.wantErr)
				}
				if len(lp.pods) != 0 {
					t.Errorf("pods left: %v", lp.pods)
				}
				for v := range lp.volumes {
					if strings.HasPrefix(v, "sbx-") {
						t.Errorf("code volume left: %s", v)
					}
				}
				if sbs, _ := m.Sandboxes(ctx, "app"); len(sbs) != 0 {
					t.Errorf("sandbox rows left: %+v", sbs)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got := lp.bodies["container"]["image"]; got != ref {
				t.Errorf("container image = %v, want %s", got, ref)
			}
			if !lp.pods[PodName(sb.ID)] {
				t.Errorf("pod %s missing", PodName(sb.ID))
			}
		})
	}
}

func TestRefreshImage(t *testing.T) {
	const cf = "FROM docker.io/library/debian:trixie-slim\n"
	tests := []struct {
		name       string
		spec       ProjectSpec
		failBuild  bool
		wantPulls  []string
		wantBuilds int
		wantErr    bool
	}{
		{"registry pulls again", ProjectSpec{Slug: "app", ImageRef: "docker.io/library/x:1"}, false, []string{"docker.io/library/x:1 always"}, 0, false},
		{"containerfile builds", ProjectSpec{Slug: "app", Containerfile: cf}, false, nil, 1, false},
		{"failed build", ProjectSpec{Slug: "app", Containerfile: cf}, true, nil, 1, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			m, _, lp := newManager(t)
			lp.anyImage, lp.failBuild = false, tt.failBuild
			p, err := m.CreateProject(ctx, tt.spec, userName)
			if err != nil {
				t.Fatal(err)
			}
			// The image is present already; a refresh gets it anyway.
			lp.images[ProjectImage(p)] = true
			ref, id, err := m.RefreshImage(ctx, "app")
			if (err != nil) != tt.wantErr {
				t.Fatalf("RefreshImage = %q, %q, %v", ref, id, err)
			}
			if ref != ProjectImage(p) || (!tt.wantErr && id != fakeImageID) {
				t.Errorf("RefreshImage = %q, %q", ref, id)
			}
			if fmt.Sprint(lp.pulls) != fmt.Sprint(tt.wantPulls) || len(lp.builds[ref]) != tt.wantBuilds {
				t.Errorf("pulls %v builds %v", lp.pulls, lp.builds)
			}
			jobs, _ := m.Store.JobsFor(ctx, p.ID)
			last := jobs[len(jobs)-1]
			if last.Type != "project.image" || (last.Status == db.JobFailed) != tt.wantErr {
				t.Errorf("job = %+v", last)
			}
		})
	}
	m, _, _ := newManager(t)
	if _, _, err := m.RefreshImage(context.Background(), "missing"); !errors.Is(err, db.ErrNotFound) {
		t.Errorf("missing project: %v", err)
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

func TestSeedSandbox(t *testing.T) {
	ctx := context.Background()
	m, _, lp := newManager(t)
	mirrors := m.Mirrors.(*fakeMirrors)
	p, err := m.CreateProject(ctx, ProjectSpec{Slug: "app", ImageRef: "img",
		RepoURL: "git@git.example.com:team/app.git", BaseBranch: "develop"}, userName)
	if err != nil {
		t.Fatal(err)
	}
	if len(mirrors.updates) != 0 {
		t.Fatal("project create contacted the origin")
	}
	sb, err := m.CreateSandbox(ctx, "app", "main")
	if err != nil {
		t.Fatal(err)
	}
	if sb.Branch != "agent/main" {
		t.Errorf("branch = %q", sb.Branch)
	}
	if env, _ := lp.bodies["container"]["env"].(map[string]any); env["WSM_BRANCH"] != "agent/main" {
		t.Errorf("container env = %v", env)
	}
	if len(lp.execs) != 0 || len(mirrors.updates) != 0 {
		t.Fatalf("sandbox create seeded: execs %v updates %d", lp.execs, len(mirrors.updates))
	}

	// The first seed fails in the container: /workspace is not empty.
	lp.execFail["git clone"] = execOutcome{128, "fatal: destination path '/workspace' already exists and is not an empty directory.\x1b[2J\n"}
	err = m.StartSandbox(ctx, "app", "main")
	if err == nil || !strings.Contains(err.Error(), "not an empty directory") || !strings.Contains(err.Error(), "re-create the sandbox") {
		t.Fatalf("StartSandbox = %v, want the clone error and the re-create hint", err)
	}
	if strings.Contains(err.Error(), "\x1b") {
		t.Errorf("error passes a control character: %q", err)
	}
	got, _ := m.Store.SandboxBySlug(ctx, p.ID, "main")
	if got.Status != db.StatusFailed || got.SeededAt != nil {
		t.Errorf("after a failed seed: status %s seeded %v", got.Status, got.SeededAt)
	}
	if fmt.Sprint(lp.podsStopped) != "[sbx-"+sb.ID+"]" {
		t.Errorf("pods stopped = %v, want the sandbox's pod", lp.podsStopped)
	}
	wantFailed := []string{
		"true",
		"dd of=/tmp/.wsm-seed.bundle status=none",
		"git clone --quiet --branch develop /tmp/.wsm-seed.bundle /workspace",
		"rm -f /tmp/.wsm-seed.bundle",
	}
	if fmt.Sprint(lp.execs) != fmt.Sprint(wantFailed) {
		t.Errorf("execs = %q\nwant %q", lp.execs, wantFailed)
	}

	// The next start tries again, and succeeds.
	delete(lp.execFail, "git clone")
	lp.execs, lp.execStdin = nil, map[int][]byte{}
	if err := m.StartSandbox(ctx, "app", "main"); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"true",
		"dd of=/tmp/.wsm-seed.bundle status=none",
		"git clone --quiet --branch develop /tmp/.wsm-seed.bundle /workspace",
		"git -C /workspace switch --quiet --create agent/main",
		"git -C /workspace remote remove origin",
		"rm -f /tmp/.wsm-seed.bundle",
	}
	if fmt.Sprint(lp.execs) != fmt.Sprint(want) {
		t.Errorf("execs = %q\nwant %q", lp.execs, want)
	}
	if string(lp.execStdin[1]) != string(mirrors.bundle) {
		t.Errorf("dd got stdin %q, want the bundle", lp.execStdin[1])
	}
	if len(lp.execStdin) != 1 {
		t.Errorf("stdin attached to %d execs, want only dd", len(lp.execStdin))
	}
	for _, call := range lp.execByID {
		if call.cmd[0] == "sh" || call.cmd[0] == "bash" {
			t.Errorf("exec through a shell: %q", call.cmd)
		}
	}
	got, _ = m.Store.SandboxBySlug(ctx, p.ID, "main")
	if got.Status != db.StatusRunning || got.SeededAt == nil {
		t.Errorf("after the seed: status %s seeded %v", got.Status, got.SeededAt)
	}
	if len(mirrors.updates) != 2 {
		t.Fatalf("mirror updates = %d, want one per seed attempt", len(mirrors.updates))
	}
	u := mirrors.updates[1]
	if u.ProjectID != p.ID || u.URL != p.RepoURL || !strings.HasPrefix(string(u.Key), "-----BEGIN OPENSSH PRIVATE KEY-----") {
		t.Errorf("update = %s %s, key ok %v", u.ProjectID, u.URL, strings.HasPrefix(string(u.Key), "-----BEGIN OPENSSH"))
	}
	if mirrors.bundles[len(mirrors.bundles)-1] != p.ID+" develop" {
		t.Errorf("bundles = %v", mirrors.bundles)
	}

	// A seeded sandbox is not seeded again.
	lp.execs = nil
	if err := m.StopSandbox(ctx, "app", "main"); err != nil {
		t.Fatal(err)
	}
	if err := m.StartSandbox(ctx, "app", "main"); err != nil {
		t.Fatal(err)
	}
	if len(lp.execs) != 0 || len(mirrors.updates) != 2 {
		t.Errorf("second start seeded again: execs %v updates %d", lp.execs, len(mirrors.updates))
	}

	// A failing mirror update fails the start before any exec.
	if _, err := m.CreateSandbox(ctx, "app", "second"); err != nil {
		t.Fatal(err)
	}
	mirrors.failUpdate = true
	if err := m.StartSandbox(ctx, "app", "second"); err == nil || !strings.Contains(err.Error(), "Permission denied") {
		t.Errorf("StartSandbox with a failing mirror = %v", err)
	}
	if len(lp.execs) != 0 {
		t.Errorf("execs after a failed mirror update: %v", lp.execs)
	}
	sb2, _ := m.Store.SandboxBySlug(ctx, p.ID, "second")
	if sb2.Status != db.StatusFailed {
		t.Errorf("status = %s, want failed", sb2.Status)
	}

	// Deleting the project removes its mirror.
	if err := m.DeleteProject(ctx, "app"); err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(mirrors.removed) != "["+p.ID+"]" {
		t.Errorf("mirrors removed = %v", mirrors.removed)
	}
}

// TestNoSeedWithoutRepo checks that a project without a repository starts
// without any exec and has no WSM_BRANCH.
func TestNoSeedWithoutRepo(t *testing.T) {
	ctx := context.Background()
	m, _, lp := newManager(t)
	if _, err := m.CreateProject(ctx, ProjectSpec{Slug: "app", ImageRef: "img"}, userName); err != nil {
		t.Fatal(err)
	}
	if _, err := m.CreateSandbox(ctx, "app", "main"); err != nil {
		t.Fatal(err)
	}
	if env, _ := lp.bodies["container"]["env"].(map[string]any); env["WSM_BRANCH"] != nil {
		t.Errorf("WSM_BRANCH set without a repository: %v", env)
	}
	if err := m.StartSandbox(ctx, "app", "main"); err != nil {
		t.Fatal(err)
	}
	if len(lp.execs) != 0 || len(m.Mirrors.(*fakeMirrors).updates) != 0 {
		t.Errorf("seeded without a repository: %v", lp.execs)
	}
	if err := m.DeleteProject(ctx, "app"); err != nil {
		t.Fatal(err)
	}
	if n := len(m.Mirrors.(*fakeMirrors).removed); n != 0 {
		t.Errorf("mirror removed for a project without a repository")
	}
}
