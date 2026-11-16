package db

import (
	"context"
	"reflect"
	"testing"
)

func TestSandboxServicesStore(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	mkProject := func(slug string) *Project {
		p := newProject(slug)
		if err := s.CreateProject(ctx, p, userName); err != nil {
			t.Fatal(err)
		}
		return p
	}
	mkSandbox := func(p *Project, slug string) *Sandbox {
		sb := &Sandbox{ProjectID: p.ID, Slug: slug}
		if err := s.CreateSandbox(ctx, sb, podName); err != nil {
			t.Fatal(err)
		}
		return sb
	}
	pa, pb := mkProject("a"), mkProject("b")
	a1, a2, a3 := mkSandbox(pa, "one"), mkSandbox(pa, "two"), mkSandbox(pa, "three")
	b1 := mkSandbox(pb, "one")

	set := func(sb *Sandbox, tag string) {
		t.Helper()
		if err := s.SetSandboxService(ctx, sb.ID, "redis", tag); err != nil {
			t.Fatalf("SetSandboxService(%s, %s): %v", sb.Slug, tag, err)
		}
	}
	set(a1, "8.8")
	set(a1, "8.10") // update
	set(a2, "8.8")
	set(b1, "7.2")
	if err := s.SetSandboxService(ctx, "bad", "redis", "8.8"); err == nil {
		t.Error("SetSandboxService accepted a bad sandbox ID")
	}

	got, err := s.SandboxServices(ctx, a1.ID)
	if err != nil || len(got) != 1 || got[0].BlockID != "redis" || got[0].Tag != "8.10" || got[0].SandboxID != a1.ID || got[0].CreatedAt.IsZero() {
		t.Fatalf("SandboxServices(a1) = %+v, %v", got, err)
	}
	if got, err := s.SandboxServices(ctx, a3.ID); err != nil || len(got) != 0 {
		t.Errorf("SandboxServices(a3) = %+v, %v", got, err)
	}

	tests := []struct {
		name string
		proj string
		tags []string
		want []string
	}{
		{"both inside", pa.ID, []string{"8.10", "8.8"}, nil},
		{"drop 8.8", pa.ID, []string{"8.10"}, []string{"two"}},
		{"drop 8.10", pa.ID, []string{"8.8"}, []string{"one"}},
		{"whole block", pa.ID, nil, []string{"one", "two"}},
		{"other project", pb.ID, []string{"8.10"}, []string{"one"}},
	}
	for _, tt := range tests {
		got, err := s.SandboxServicesOutsideCeiling(ctx, tt.proj, "redis", tt.tags)
		if err != nil || !reflect.DeepEqual(got, tt.want) {
			t.Errorf("%s: = %v, %v; want %v", tt.name, got, err, tt.want)
		}
	}
	if got, err := s.SandboxServicesOutsideCeiling(ctx, pa.ID, "mariadb", nil); err != nil || got != nil {
		t.Errorf("other block: = %v, %v", got, err)
	}

	// Deleting the sandbox removes its row (cascade).
	if err := s.DeleteSandbox(ctx, a1.ID); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := s.DB.QueryRowContext(ctx, `SELECT count(*) FROM sandbox_services WHERE sandbox_id = ?`, a1.ID).Scan(&n); err != nil || n != 0 {
		t.Errorf("rows after delete = %d, %v", n, err)
	}
	if got, _ := s.SandboxServicesOutsideCeiling(ctx, pa.ID, "redis", nil); !reflect.DeepEqual(got, []string{"two"}) {
		t.Errorf("after delete: %v", got)
	}
}
