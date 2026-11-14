package api

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/JCombee/wsm/internal/db"
	"github.com/JCombee/wsm/internal/podman"
	"github.com/JCombee/wsm/internal/podman/podmantest"
	"github.com/JCombee/wsm/internal/sandbox"
)

type nopUsers struct{}

func (nopUsers) CreateProjectUser(context.Context, int) error { return nil }
func (nopUsers) DeleteProjectUser(context.Context, int) error { return nil }

const testImageID = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

// okLibpod answers every libpod call with a plausible success.
func okLibpod(w http.ResponseWriter, r *http.Request) {
	p := r.URL.Path
	switch {
	case strings.HasSuffix(p, "/exists"):
		w.WriteHeader(http.StatusNoContent)
	case strings.Contains(p, "/images/") && strings.HasSuffix(p, "/json"):
		io.WriteString(w, `{"Id":"`+testImageID+`","Config":{"Cmd":["bash"]}}`)
	case strings.HasSuffix(p, "/images/pull"), strings.HasSuffix(p, "/build"):
		io.Copy(io.Discard, r.Body)
		io.WriteString(w, `{"stream":"done\n"}`)
	case strings.HasSuffix(p, "/start"), strings.HasSuffix(p, "/stop"):
		io.WriteString(w, `{"Errs":[],"Id":"x"}`)
	case r.Method == http.MethodDelete && strings.Contains(p, "/pods/"):
		io.WriteString(w, `[]`)
	case r.Method == http.MethodDelete:
		w.WriteHeader(http.StatusNoContent)
	default:
		w.WriteHeader(http.StatusCreated)
		io.WriteString(w, `{"Id":"x"}`)
	}
}

func newTestAPI(t *testing.T) (*Client, string) {
	t.Helper()
	sqlDB, err := db.Open(context.Background(), filepath.Join(t.TempDir(), "wsm.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { sqlDB.Close() })
	lp := podmantest.New(t, http.HandlerFunc(okLibpod))
	s := &Server{
		Manager: &sandbox.Manager{
			Store:  db.NewStore(sqlDB),
			Users:  nopUsers{},
			Podman: func(int) *podman.Client { return lp.Client() },
			Logf:   t.Logf,
		},
		UserName: func(uid int) string { return fmt.Sprintf("wsm-p-%d", uid) },
		Logf:     t.Logf,
	}
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)
	addr := strings.TrimPrefix(srv.URL, "http://")
	c := NewClientDialer(func(ctx context.Context) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "tcp", addr)
	})
	return c, srv.URL
}

func TestProjectAndSandboxRoutes(t *testing.T) {
	ctx := context.Background()
	c, _ := newTestAPI(t)

	p, err := c.CreateProject(ctx, CreateProjectRequest{Slug: "app", Image: "localhost/wsm-test:latest"})
	if err != nil {
		t.Fatal(err)
	}
	if p.LinuxUID != 50000 || p.CPUMillis != sandbox.DefaultCPUMillis {
		t.Errorf("project %+v", p)
	}
	if _, err := c.CreateProject(ctx, CreateProjectRequest{Slug: "app", Image: "img"}); err == nil ||
		!strings.Contains(err.Error(), "already exists") {
		t.Errorf("duplicate project: %v", err)
	}
	if _, err := c.CreateSandbox(ctx, "app", "main"); err != nil {
		t.Fatal(err)
	}
	if err := c.StartSandbox(ctx, "app", "main"); err != nil {
		t.Fatal(err)
	}
	sbs, err := c.Sandboxes(ctx, "app")
	if err != nil || len(sbs) != 1 || sbs[0].Status != "running" || sbs[0].Project != "app" {
		t.Fatalf("Sandboxes = %+v, %v", sbs, err)
	}
	if err := c.StopSandbox(ctx, "app", "main"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Terminal(ctx, "app", "main", 24, 80); err == nil || !strings.Contains(err.Error(), "stopped") {
		t.Errorf("terminal on stopped sandbox: %v", err)
	}
	if err := c.StartSandbox(ctx, "app", "nope"); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Errorf("start missing sandbox: %v", err)
	}
	if err := c.DeleteSandbox(ctx, "app", "main"); err != nil {
		t.Fatal(err)
	}
	if err := c.DeleteProject(ctx, "app"); err != nil {
		t.Fatal(err)
	}
	if ps, err := c.Projects(ctx); err != nil || len(ps) != 0 {
		t.Errorf("Projects = %+v, %v", ps, err)
	}
}

func TestProjectImageRoutes(t *testing.T) {
	ctx := context.Background()
	c, _ := newTestAPI(t)
	// The largest Containerfile, full of characters JSON escapes, fits.
	cf := "FROM docker.io/library/debian:trixie-slim\n#" + strings.Repeat("\x01", db.MaxContainerfile-44) + "\n"
	p, err := c.CreateProject(ctx, CreateProjectRequest{Slug: "built", Containerfile: cf})
	if err != nil {
		t.Fatal(err)
	}
	if p.ImageSource != db.ImageSourceContainerfile || !strings.HasPrefix(p.Image, "localhost/wsm-"+p.ID+":") {
		t.Errorf("project %+v", p)
	}
	if _, err := c.CreateProject(ctx, CreateProjectRequest{Slug: "pulled", Image: "docker.io/library/debian:trixie-slim"}); err != nil {
		t.Fatal(err)
	}
	for _, slug := range []string{"built", "pulled"} {
		img, err := c.ProjectImage(ctx, slug)
		if err != nil || img.ID != testImageID || img.Image == "" {
			t.Errorf("ProjectImage(%s) = %+v, %v", slug, img, err)
		}
	}
	if _, err := c.ProjectImage(ctx, "none"); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Errorf("image of missing project: %v", err)
	}
	ps, err := c.Projects(ctx)
	if err != nil || len(ps) != 2 || ps[0].ImageSource != "containerfile" || ps[1].ImageSource != "registry" ||
		ps[1].Image != "docker.io/library/debian:trixie-slim" {
		t.Errorf("Projects = %+v, %v", ps, err)
	}
}

func TestRejectsInvalidBodies(t *testing.T) {
	_, url := newTestAPI(t)
	tests := []struct {
		name, path, body string
		want             int
	}{
		{"unknown field", "/v1/projects", `{"slug":"app","image":"img","privileged":true}`, 400},
		{"wrong type", "/v1/projects", `{"slug":"app","image":"img","cpu_millis":"lots"}`, 400},
		{"two objects", "/v1/projects", `{"slug":"app","image":"img"}{}`, 400},
		{"too large", "/v1/projects", `{"slug":"app","name":"` + strings.Repeat("a", maxProjectBody) + `"}`, 400},
		{"sandbox body too large", "/v1/projects/app/sandboxes", `{"slug":"` + strings.Repeat("a", maxRequestBody) + `"}`, 400},
		{"image and containerfile", "/v1/projects", `{"slug":"app","image":"img","containerfile":"FROM img\n"}`, 400},
		{"containerfile with NUL", "/v1/projects", `{"slug":"app","containerfile":"FROM img\u0000\n"}`, 400},
		{"bad slug", "/v1/projects", `{"slug":"../etc","image":"img"}`, 400},
		{"sandbox in missing project", "/v1/projects/none/sandboxes", `{"slug":"main"}`, 404},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resp, err := http.Post(url+tt.path, "application/json", strings.NewReader(tt.body))
			if err != nil {
				t.Fatal(err)
			}
			resp.Body.Close()
			if resp.StatusCode != tt.want {
				t.Errorf("status %d, want %d", resp.StatusCode, tt.want)
			}
		})
	}
}
