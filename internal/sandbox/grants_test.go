package sandbox

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/JCombee/wsm/internal/db"
)

// fakeGrants records, at each revoke, what state the sandbox was in.
type fakeGrants struct {
	mu     sync.Mutex
	m      *Manager
	lp     *fakeLibpod
	users  *fakeUsers
	fail   bool
	events []string
}

func (f *fakeGrants) RevokeSandbox(ctx context.Context, sandboxID string) error {
	sb, err := f.m.Store.SandboxByID(ctx, sandboxID)
	f.lp.mu.Lock()
	pod := f.lp.pods["sbx-"+sandboxID]
	f.lp.mu.Unlock()
	f.users.mu.Lock()
	userCalls := strings.Join(f.users.calls, ",")
	f.users.mu.Unlock()
	f.mu.Lock()
	defer f.mu.Unlock()
	f.events = append(f.events, fmt.Sprintf("revoke row=%v token=%v pod=%v users=%s",
		err == nil, sb.MCPTokenHash != "", pod, userCalls))
	if f.fail {
		return errors.New("instance down")
	}
	return nil
}

func (f *fakeGrants) take() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := f.events
	f.events = nil
	return out
}

func TestDeleteSandboxRevokes(t *testing.T) {
	ctx := context.Background()
	m, users, lp := newManager(t)
	g := &fakeGrants{m: m, lp: lp, users: users}
	m.Grants = g
	if _, err := m.CreateProject(ctx, ProjectSpec{Slug: "app", ImageRef: "localhost/wsm-test:latest",
		CPUMillis: 1000, MemBytes: 512 << 20, PidsLimit: 256}, userName); err != nil {
		t.Fatal(err)
	}
	sb, err := m.CreateSandbox(ctx, "app", "main")
	if err != nil {
		t.Fatal(err)
	}
	if sb.MCPTokenHash == "" {
		t.Fatal("new sandbox has no token hash")
	}

	// Revoking fails: an error, the row stays, the token is gone.
	g.fail = true
	if err := m.DeleteSandbox(ctx, "app", "main"); err == nil || !strings.Contains(err.Error(), "instance down") {
		t.Fatalf("DeleteSandbox with a failing revoke: %v", err)
	}
	// Order: token cleared, pod gone, then revoke while the row exists.
	if ev := g.take(); len(ev) != 1 || ev[0] != "revoke row=true token=false pod=false users=create 50000" {
		t.Errorf("events = %q", ev)
	}
	left, err := m.Store.SandboxByID(ctx, sb.ID)
	if err != nil {
		t.Fatalf("row gone after a failed revoke: %v", err)
	}
	if left.MCPTokenHash != "" {
		t.Error("token hash kept after a failed delete")
	}

	// A second delete finishes; the pod being gone is no error.
	g.fail = false
	if err := m.DeleteSandbox(ctx, "app", "main"); err != nil {
		t.Fatalf("second DeleteSandbox: %v", err)
	}
	if ev := g.take(); len(ev) != 1 {
		t.Errorf("events = %q", ev)
	}
	if _, err := m.Store.SandboxByID(ctx, sb.ID); !errors.Is(err, db.ErrNotFound) {
		t.Errorf("row after delete: %v", err)
	}

	// DeleteProject revokes every sandbox before the project user goes,
	// and stops when a revoke fails.
	for _, slug := range []string{"one", "two"} {
		if _, err := m.CreateSandbox(ctx, "app", slug); err != nil {
			t.Fatal(err)
		}
	}
	g.fail = true
	if err := m.DeleteProject(ctx, "app"); err == nil {
		t.Fatal("DeleteProject with a failing revoke succeeded")
	}
	if got := strings.Join(users.calls, ","); got != "create 50000" {
		t.Errorf("project user deleted although a revoke failed: %s", got)
	}
	if sbs, _ := m.Sandboxes(ctx, "app"); len(sbs) != 2 {
		t.Errorf("sandboxes after a failed project delete: %d", len(sbs))
	}
	g.take()
	g.fail = false
	if err := m.DeleteProject(ctx, "app"); err != nil {
		t.Fatalf("DeleteProject: %v", err)
	}
	ev := g.take()
	if len(ev) != 2 {
		t.Fatalf("events = %q", ev)
	}
	for _, e := range ev {
		if e != "revoke row=true token=false pod=false users=create 50000" {
			t.Errorf("revoke not before the user delete: %s", e)
		}
	}
	if got := strings.Join(users.calls, ","); got != "create 50000,delete 50000" {
		t.Errorf("user calls = %s", got)
	}
}
