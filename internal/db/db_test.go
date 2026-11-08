package db

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"testing"
	"time"
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
	for i, want := range []int{50000, 50001, 50002} {
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
	if p.LinuxUID != 50001 {
		t.Errorf("reused uid = %d, want 50001", p.LinuxUID)
	}
}

func TestProjectUIDRangeExhausted(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	// Fill the range directly; creating 10000 projects one by one is slow.
	_, err := s.DB.Exec(`
		WITH RECURSIVE n(u) AS (SELECT 50000 UNION ALL SELECT u + 1 FROM n WHERE u < 59999)
		INSERT INTO projects (id, slug, name, linux_user, linux_uid, image_ref,
			cpu_limit, mem_limit, pids_limit, created_at)
		SELECT 'id' || u, 's' || u, 'n', 'wsm-p-' || u, u, 'img', 1, 1, 1, '2026-01-01T00:00:00.000000000Z' FROM n`)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.CreateProject(ctx, newProject("full"), userName); err == nil {
		t.Fatal("CreateProject succeeded with a full UID range")
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
