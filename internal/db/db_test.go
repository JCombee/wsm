package db

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pressly/goose/v3"

	"github.com/JCombee/wsm/internal/projectuid"
)

func openTest(t *testing.T) *Store {
	t.Helper()
	sqlDB, err := Open(context.Background(), filepath.Join(t.TempDir(), "wsm.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { sqlDB.Close() })
	return NewStore(sqlDB)
}

func userName(uid int) string  { return fmt.Sprintf("wsm-p-%d", uid) }
func podName(id string) string { return "sbx-" + id }

func newProject(slug string) *Project {
	return &Project{Slug: slug, Name: slug, ImageRef: "localhost/test:latest",
		CPUMillis: 1000, MemBytes: 512 << 20, PidsLimit: 256}
}

func TestMigrateIsIdempotent(t *testing.T) {
	s := openTest(t)
	if err := Migrate(context.Background(), s.DB); err != nil {
		t.Fatalf("second Migrate: %v", err)
	}
	for _, table := range []string{"projects", "sandboxes", "audit_events", "jobs"} {
		var n int
		err := s.DB.QueryRow(`SELECT count(*) FROM sqlite_schema WHERE type = 'table' AND name = ?`, table).Scan(&n)
		if err != nil || n != 1 {
			t.Errorf("table %s: n=%d err=%v", table, n, err)
		}
	}
}

func TestNewID(t *testing.T) {
	seen := map[string]bool{}
	for range 1000 {
		id, err := NewID()
		if err != nil {
			t.Fatal(err)
		}
		if !ValidID(id) {
			t.Fatalf("NewID() = %q, not valid", id)
		}
		if seen[id] {
			t.Fatalf("duplicate id %q", id)
		}
		seen[id] = true
	}
	for _, s := range []string{"", "ABCDEFGHIJKL", "abcdefghijk", "abcdefghijklm", "abcdefghijk1", "abc/efghijkl"} {
		if ValidID(s) {
			t.Errorf("ValidID(%q) = true", s)
		}
	}
}

func TestProjectUIDAllocation(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	var ps []*Project
	for i, want := range []int{projectuid.Min, projectuid.Min + 1, projectuid.Min + 2} {
		p := newProject(fmt.Sprintf("p%d", i))
		if err := s.CreateProject(ctx, p, userName); err != nil {
			t.Fatalf("CreateProject: %v", err)
		}
		if p.LinuxUID != want || p.LinuxUser != userName(want) {
			t.Errorf("project %d: uid %d user %q, want %d", i, p.LinuxUID, p.LinuxUser, want)
		}
		ps = append(ps, p)
	}
	// A freed UID is the lowest free one again.
	if err := s.DeleteProject(ctx, ps[1].ID); err != nil {
		t.Fatal(err)
	}
	p := newProject("again")
	if err := s.CreateProject(ctx, p, userName); err != nil {
		t.Fatal(err)
	}
	if p.LinuxUID != projectuid.Min+1 {
		t.Errorf("reused uid = %d, want %d", p.LinuxUID, projectuid.Min+1)
	}
}

func TestProjectUIDRangeExhausted(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	// Fill the range directly; creating 10000 projects one by one is slow.
	_, err := s.DB.Exec(`
		WITH RECURSIVE n(u) AS (SELECT ? UNION ALL SELECT u + 1 FROM n WHERE u < ?)
		INSERT INTO projects (id, slug, name, linux_user, linux_uid, image_ref,
			cpu_limit, mem_limit, pids_limit, created_at)
		SELECT 'id' || u, 's' || u, 'n', 'wsm-p-' || u, u, 'img', 1, 1, 1, '2026-01-01T00:00:00.000000000Z' FROM n`,
		projectuid.Min, projectuid.Max)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.CreateProject(ctx, newProject("full"), userName); err == nil {
		t.Fatal("CreateProject succeeded with a full UID range")
	}
}

// TestUIDCheckMatchesRange keeps the CHECK constraint on projects.linux_uid
// equal to internal/projectuid, the range wsm-helper and the nftables rules
// use.
func TestUIDCheckMatchesRange(t *testing.T) {
	s := openTest(t)
	tests := []struct {
		uid int
		ok  bool
	}{
		{projectuid.Min - 1, false},
		{projectuid.Min, true},
		{projectuid.Max, true},
		{projectuid.Max + 1, false},
	}
	for _, tt := range tests {
		_, err := s.DB.Exec(`
			INSERT INTO projects (id, slug, name, linux_user, linux_uid, image_ref,
				cpu_limit, mem_limit, pids_limit, created_at)
			VALUES (?, ?, 'n', ?, ?, 'img', 1, 1, 1, '2026-01-01T00:00:00.000000000Z')`,
			fmt.Sprint("id", tt.uid), fmt.Sprint("s", tt.uid), userName(tt.uid), tt.uid)
		if (err == nil) != tt.ok {
			t.Errorf("insert uid %d: err = %v, want ok = %v", tt.uid, err, tt.ok)
		}
	}
}

func TestProjectConstraints(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	if err := s.CreateProject(ctx, newProject("dup"), userName); err != nil {
		t.Fatal(err)
	}
	err := s.CreateProject(ctx, newProject("dup"), userName)
	if !errors.Is(err, ErrConflict) {
		t.Errorf("duplicate slug: err = %v, want ErrConflict", err)
	}
	_, err = s.ProjectBySlug(ctx, "missing")
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("missing project: err = %v, want ErrNotFound", err)
	}
	if err := s.DeleteProject(ctx, "missing"); !errors.Is(err, ErrNotFound) {
		t.Errorf("delete missing: err = %v", err)
	}
	// Limits must be positive.
	bad := newProject("bad")
	bad.MemBytes = 0
	if err := s.CreateProject(ctx, bad, userName); err == nil {
		t.Error("CreateProject accepted mem_limit 0")
	}
}

func TestSandboxLifecycle(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	now := time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)
	s.Now = func() time.Time { return now }

	p := newProject("proj")
	if err := s.CreateProject(ctx, p, userName); err != nil {
		t.Fatal(err)
	}
	sb := &Sandbox{ProjectID: p.ID, Slug: "main"}
	if err := s.CreateSandbox(ctx, sb, podName); err != nil {
		t.Fatal(err)
	}
	if sb.PodName != "sbx-"+sb.ID || sb.Status != StatusCreated {
		t.Errorf("sandbox = %+v", sb)
	}
	if err := s.CreateSandbox(ctx, &Sandbox{ProjectID: p.ID, Slug: "main"}, podName); !errors.Is(err, ErrConflict) {
		t.Errorf("duplicate sandbox slug: err = %v", err)
	}
	if err := s.CreateSandbox(ctx, &Sandbox{ProjectID: "nope", Slug: "x"}, podName); err == nil {
		t.Error("sandbox for a missing project was accepted")
	}

	// A project with sandboxes cannot be deleted.
	if err := s.DeleteProject(ctx, p.ID); err == nil {
		t.Error("DeleteProject succeeded while a sandbox exists")
	}

	if err := s.SetSandboxStatus(ctx, sb.ID, StatusRunning); err != nil {
		t.Fatal(err)
	}
	if err := s.TouchSandbox(ctx, sb.ID); err != nil {
		t.Fatal(err)
	}
	got, err := s.SandboxBySlug(ctx, p.ID, "main")
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != StatusRunning || got.LastActivityAt == nil || !got.LastActivityAt.Equal(now) {
		t.Errorf("after update: %+v", got)
	}
	list, err := s.Sandboxes(ctx, "")
	if err != nil || len(list) != 1 {
		t.Fatalf("Sandboxes = %v, %v", list, err)
	}
	if err := s.DeleteSandbox(ctx, sb.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteProject(ctx, p.ID); err != nil {
		t.Fatal(err)
	}
}

func TestJobs(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	ok, err := s.StartJob(ctx, "sandbox.create", "ref1")
	if err != nil {
		t.Fatal(err)
	}
	bad, err := s.StartJob(ctx, "sandbox.start", "ref1")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.FinishJob(ctx, ok, nil); err != nil {
		t.Fatal(err)
	}
	if err := s.FinishJob(ctx, bad, errors.New("boom")); err != nil {
		t.Fatal(err)
	}
	jobs, err := s.JobsFor(ctx, "ref1")
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 2 {
		t.Fatalf("got %d jobs", len(jobs))
	}
	byID := map[string]Job{jobs[0].ID: jobs[0], jobs[1].ID: jobs[1]}
	if j := byID[ok]; j.Status != JobSucceeded || j.FinishedAt == nil || j.Error != "" {
		t.Errorf("ok job = %+v", j)
	}
	if j := byID[bad]; j.Status != JobFailed || j.Error != "boom" {
		t.Errorf("failed job = %+v", j)
	}
}

func TestTimeFormatSorts(t *testing.T) {
	a := formatTime(time.Date(2026, 1, 1, 0, 0, 5, 0, time.UTC))
	b := formatTime(time.Date(2026, 1, 1, 0, 0, 5, 100, time.UTC))
	if !(a < b) {
		t.Errorf("%q does not sort before %q", a, b)
	}
	got, err := parseTime(b)
	if err != nil || !got.Equal(time.Date(2026, 1, 1, 0, 0, 5, 100, time.UTC)) {
		t.Errorf("round trip: %v, %v", got, err)
	}
}

// migrateTo applies the embedded migrations up to and including version.
func migrateTo(t *testing.T, sqlDB *sql.DB, version int64) {
	t.Helper()
	fsys, err := fs.Sub(migrations, "migrations")
	if err != nil {
		t.Fatal(err)
	}
	p, err := goose.NewProvider(goose.DialectSQLite3, sqlDB, fsys, goose.WithDisableGlobalRegistry(true))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.UpTo(context.Background(), version); err != nil {
		t.Fatal(err)
	}
}

func TestProjectImageSource(t *testing.T) {
	ctx := context.Background()
	const cf = "FROM docker.io/library/debian:trixie-slim\nCMD [\"sleep\",\"infinity\"]\n"
	tests := []struct {
		name          string
		source        string
		imageRef      string
		containerfile string
		wantErr       bool
		wantSource    string
	}{
		{"registry", ImageSourceRegistry, "docker.io/library/debian:trixie-slim", "", false, ImageSourceRegistry},
		{"empty source means registry", "", "localhost/test:latest", "", false, ImageSourceRegistry},
		{"containerfile", ImageSourceContainerfile, "", cf, false, ImageSourceContainerfile},
		{"containerfile at the limit", ImageSourceContainerfile, "", "FROM x\n" + strings.Repeat("#", MaxContainerfile-7), false, ImageSourceContainerfile},
		{"unknown source", "git", "img", "", true, ""},
		{"registry without image", ImageSourceRegistry, "", "", true, ""},
		{"registry with containerfile", ImageSourceRegistry, "img", cf, true, ""},
		{"both sources", ImageSourceContainerfile, "img", cf, true, ""},
		{"containerfile missing", ImageSourceContainerfile, "", "", true, ""},
		{"containerfile blank", ImageSourceContainerfile, "", " \n\t", true, ""},
		{"containerfile too large", ImageSourceContainerfile, "", "FROM x\n" + strings.Repeat("#", MaxContainerfile-6), true, ""},
		{"containerfile with NUL", ImageSourceContainerfile, "", "FROM x\x00\n", true, ""},
		{"containerfile not UTF-8", ImageSourceContainerfile, "", "FROM x\xff\n", true, ""},
	}
	for i, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := openTest(t)
			p := newProject(fmt.Sprintf("p%d", i))
			p.ImageSource, p.ImageRef, p.Containerfile = tt.source, tt.imageRef, tt.containerfile
			err := s.CreateProject(ctx, p, userName)
			if tt.wantErr {
				if !errors.Is(err, ErrInvalid) {
					t.Fatalf("err = %v, want ErrInvalid", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			got, err := s.ProjectByID(ctx, p.ID)
			if err != nil {
				t.Fatal(err)
			}
			if got.ImageSource != tt.wantSource || got.ImageRef != tt.imageRef || got.Containerfile != tt.containerfile {
				t.Errorf("stored source %q ref %q containerfile %d bytes", got.ImageSource, got.ImageRef, len(got.Containerfile))
			}
		})
	}

	// The CHECK constraints hold even for writes that bypass the store.
	raw := []struct {
		name          string
		source        string
		imageRef      string
		containerfile any
		ok            bool
	}{
		{"registry", ImageSourceRegistry, "img", nil, true},
		{"containerfile", ImageSourceContainerfile, "", cf, true},
		{"unknown source", "git", "img", nil, false},
		{"registry with containerfile", ImageSourceRegistry, "img", cf, false},
		{"registry without image", ImageSourceRegistry, "", nil, false},
		{"containerfile with image", ImageSourceContainerfile, "img", cf, false},
		{"containerfile missing", ImageSourceContainerfile, "", nil, false},
		{"containerfile empty", ImageSourceContainerfile, "", "", false},
		{"containerfile too large", ImageSourceContainerfile, "", strings.Repeat("#", MaxContainerfile+1), false},
	}
	s := openTest(t)
	for i, tt := range raw {
		t.Run("check/"+tt.name, func(t *testing.T) {
			_, err := s.DB.Exec(`
				INSERT INTO projects (id, slug, name, linux_user, linux_uid, image_source, image_ref,
					containerfile, cpu_limit, mem_limit, pids_limit, created_at)
				VALUES (?, ?, 'n', ?, ?, ?, ?, ?, 1, 1, 1, '2026-01-01T00:00:00.000000000Z')`,
				fmt.Sprint("id", i), fmt.Sprint("s", i), userName(projectuid.Min+i), projectuid.Min+i,
				tt.source, tt.imageRef, tt.containerfile)
			if (err == nil) != tt.ok {
				t.Fatalf("insert: err = %v, want ok = %v", err, tt.ok)
			}
		})
	}

	// Projects from before migration 00003 become registry projects.
	t.Run("existing projects", func(t *testing.T) {
		sqlDB, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "old.db"))
		if err != nil {
			t.Fatal(err)
		}
		defer sqlDB.Close()
		migrateTo(t, sqlDB, 2)
		_, err = sqlDB.Exec(`
			INSERT INTO projects (id, slug, name, linux_user, linux_uid, image_ref,
				cpu_limit, mem_limit, pids_limit, created_at)
			VALUES ('old', 'old', 'n', 'wsm-p-50000', 50000, 'localhost/test:latest', 1, 1, 1,
				'2026-01-01T00:00:00.000000000Z')`)
		if err != nil {
			t.Fatal(err)
		}
		if err := Migrate(ctx, sqlDB); err != nil {
			t.Fatal(err)
		}
		got, err := NewStore(sqlDB).ProjectBySlug(ctx, "old")
		if err != nil {
			t.Fatal(err)
		}
		if got.ImageSource != ImageSourceRegistry || got.ImageRef != "localhost/test:latest" || got.Containerfile != "" {
			t.Errorf("migrated project: %+v", got)
		}
	})
}

func TestProjectRepo(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	sealed := []byte{1, 2, 3, 4}
	id, err := NewID()
	if err != nil {
		t.Fatal(err)
	}
	p := newProject("app")
	p.ID, p.RepoURL, p.BaseBranch, p.DeployKeyPub, p.DeployKeySealed =
		id, "git@host:app.git", "main", "ssh-ed25519 AAAA wsm-app", sealed
	if err := s.CreateProject(ctx, p, userName); err != nil {
		t.Fatal(err)
	}
	if p.ID != id {
		t.Errorf("ID = %s, want the preset %s", p.ID, id)
	}
	got, err := s.ProjectBySlug(ctx, "app")
	if err != nil {
		t.Fatal(err)
	}
	if got.RepoURL != p.RepoURL || got.BaseBranch != "main" || got.DeployKeyPub != p.DeployKeyPub || !got.HasRepo() {
		t.Errorf("project = %+v", got)
	}
	if got.DeployKeySealed != nil {
		t.Error("a scan returned the sealed deploy key")
	}
	if b, err := s.DeployKeySealed(ctx, id); err != nil || !bytes.Equal(b, sealed) {
		t.Errorf("DeployKeySealed = %v, %v", b, err)
	}

	plain := newProject("plain")
	if err := s.CreateProject(ctx, plain, userName); err != nil {
		t.Fatal(err)
	}
	if plain.HasRepo() {
		t.Error("project without a repository has one")
	}
	if _, err := s.DeployKeySealed(ctx, plain.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("DeployKeySealed without a repository: %v", err)
	}

	bad := []struct {
		name string
		mod  func(p *Project)
	}{
		{"repo without key", func(p *Project) { p.RepoURL, p.BaseBranch = "git@h:a.git", "main" }},
		{"repo without branch", func(p *Project) { p.RepoURL, p.DeployKeyPub, p.DeployKeySealed = "git@h:a.git", "k", sealed }},
		{"branch without repo", func(p *Project) { p.BaseBranch = "main" }},
		{"key without repo", func(p *Project) { p.DeployKeyPub, p.DeployKeySealed = "k", sealed }},
		{"invalid preset id", func(p *Project) { p.ID = "../x" }},
	}
	for i, tt := range bad {
		t.Run(tt.name, func(t *testing.T) {
			p := newProject(fmt.Sprint("bad", i))
			tt.mod(p)
			if err := s.CreateProject(ctx, p, userName); !errors.Is(err, ErrInvalid) {
				t.Errorf("err = %v, want ErrInvalid", err)
			}
		})
	}

	// The CHECK constraints hold for writes that bypass the store.
	_, err = s.DB.Exec(`
		INSERT INTO projects (id, slug, name, linux_user, linux_uid, image_ref,
			cpu_limit, mem_limit, pids_limit, created_at, repo_url)
		VALUES ('raw', 'raw', 'n', 'wsm-p-59999', 59999, 'img', 1, 1, 1,
			'2026-01-01T00:00:00.000000000Z', 'git@h:a.git')`)
	if err == nil {
		t.Error("insert of a repository without branch and key succeeded")
	}

	sb := &Sandbox{ProjectID: id, Slug: "main", Branch: "agent/main"}
	if err := s.CreateSandbox(ctx, sb, podName); err != nil {
		t.Fatal(err)
	}
	gotSB, err := s.SandboxBySlug(ctx, id, "main")
	if err != nil || gotSB.Branch != "agent/main" || gotSB.SeededAt != nil {
		t.Fatalf("sandbox = %+v, %v", gotSB, err)
	}
	if err := s.MarkSandboxSeeded(ctx, sb.ID); err != nil {
		t.Fatal(err)
	}
	if gotSB, _ = s.SandboxBySlug(ctx, id, "main"); gotSB.SeededAt == nil {
		t.Error("SeededAt not set")
	}
	if err := s.MarkSandboxSeeded(ctx, "missing"); !errors.Is(err, ErrNotFound) {
		t.Errorf("MarkSandboxSeeded on a missing sandbox: %v", err)
	}
}
