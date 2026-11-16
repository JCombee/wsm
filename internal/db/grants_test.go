package db

import (
	"bytes"
	"context"
	"errors"
	"reflect"
	"testing"
)

func TestGrantsStore(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	mkProject := func(slug string) *Project {
		p := newProject(slug)
		if err := s.CreateProject(ctx, p, userName); err != nil {
			t.Fatal(err)
		}
		return p
	}
	mkSandbox := func(p *Project, slug, hash string) *Sandbox {
		sb := &Sandbox{ProjectID: p.ID, Slug: slug, MCPTokenHash: hash}
		if err := s.CreateSandbox(ctx, sb, podName); err != nil {
			t.Fatal(err)
		}
		return sb
	}
	mkInstance := func(tag string) *ServiceInstance {
		id, _ := NewID()
		inst := &ServiceInstance{ID: id, BlockID: "mariadb", Tag: tag, SecretEnc: []byte{1}}
		if err := s.CreateServiceInstance(ctx, inst); err != nil {
			t.Fatal(err)
		}
		return inst
	}
	mkGrant := func(sb *Sandbox, inst *ServiceInstance) *Grant {
		id, _ := NewID()
		g := &Grant{ID: id, SandboxID: sb.ID, InstanceID: inst.ID, SecretEnc: []byte{7, 8}}
		if err := s.CreateGrant(ctx, g); err != nil {
			t.Fatalf("CreateGrant: %v", err)
		}
		return g
	}

	pa, pb := mkProject("a"), mkProject("b")
	a1 := mkSandbox(pa, "one", hashOf("tok-one"))
	a2 := mkSandbox(pa, "two", "")
	b1 := mkSandbox(pb, "one", "")
	i118, i123 := mkInstance("11.8"), mkInstance("12.3")

	g1 := mkGrant(a1, i118)
	mkGrant(a1, i123)
	g3 := mkGrant(a2, i118)
	mkGrant(b1, i118)

	// Unique (sandbox, instance).
	id, _ := NewID()
	if err := s.CreateGrant(ctx, &Grant{ID: id, SandboxID: a1.ID, InstanceID: i118.ID,
		SecretEnc: []byte{1}}); !errors.Is(err, ErrConflict) {
		t.Errorf("duplicate grant: err = %v, want ErrConflict", err)
	}
	if err := s.CreateGrant(ctx, &Grant{ID: id, SandboxID: "nosuchsandbx", InstanceID: i118.ID,
		SecretEnc: []byte{1}}); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown sandbox: err = %v, want ErrNotFound", err)
	}
	if err := s.CreateGrant(ctx, &Grant{ID: "bad", SandboxID: a1.ID, InstanceID: i118.ID,
		SecretEnc: []byte{1}}); !errors.Is(err, ErrInvalid) {
		t.Errorf("bad id: err = %v, want ErrInvalid", err)
	}
	if err := s.CreateGrant(ctx, &Grant{ID: id, SandboxID: a1.ID, InstanceID: i118.ID}); !errors.Is(err, ErrInvalid) {
		t.Errorf("no secret: err = %v, want ErrInvalid", err)
	}

	got, err := s.Grant(ctx, a1.ID, i118.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != g1.ID || !bytes.Equal(got.SecretEnc, g1.SecretEnc) || got.BlockID != "mariadb" ||
		got.Tag != "11.8" || got.Port != i118.Port || !got.CreatedAt.Equal(g1.CreatedAt) {
		t.Errorf("Grant = %+v, want %+v", got, g1)
	}
	if _, err := s.Grant(ctx, a2.ID, i123.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("missing grant: err = %v, want ErrNotFound", err)
	}

	list, err := s.GrantsBySandbox(ctx, a1.ID)
	if err != nil || len(list) != 2 || list[0].Tag != "11.8" || list[1].Tag != "12.3" {
		t.Errorf("GrantsBySandbox = %+v, %v", list, err)
	}

	for _, c := range []struct {
		project, instance string
		want              int
	}{{pa.ID, i118.ID, 2}, {pa.ID, i123.ID, 1}, {pb.ID, i118.ID, 1}, {pb.ID, i123.ID, 0}} {
		n, err := s.CountProjectGrants(ctx, c.project, c.instance)
		if err != nil || n != c.want {
			t.Errorf("CountProjectGrants(%s, %s) = %d, %v; want %d", c.project, c.instance, n, err, c.want)
		}
	}

	targets, err := s.GrantTargets(ctx)
	if err != nil {
		t.Fatal(err)
	}
	want := []GrantTarget{{pa.LinuxUID, i118.Port}, {pa.LinuxUID, i123.Port}, {pb.LinuxUID, i118.Port}}
	if !reflect.DeepEqual(targets, want) {
		t.Errorf("GrantTargets = %v, want %v", targets, want)
	}

	for _, c := range []struct {
		block string
		tags  []string
		want  []string
	}{
		{"mariadb", []string{"11.8", "12.3"}, nil},
		{"mariadb", []string{"11.8"}, []string{"one"}},
		{"mariadb", []string{"12.3"}, []string{"one", "two"}},
		{"mariadb", nil, []string{"one", "two"}},
		{"redis", nil, nil},
	} {
		got, err := s.GrantsOutsideCeiling(ctx, pa.ID, c.block, c.tags)
		if err != nil || !reflect.DeepEqual(got, c.want) {
			t.Errorf("GrantsOutsideCeiling(%s, %v) = %v, %v; want %v", c.block, c.tags, got, err, c.want)
		}
	}

	// A sandbox with a grant cannot be deleted.
	if err := s.DeleteSandbox(ctx, a2.ID); err == nil {
		t.Error("DeleteSandbox with a grant succeeded")
	}
	if err := s.DeleteGrant(ctx, g3.ID); err != nil {
		t.Fatalf("DeleteGrant: %v", err)
	}
	if err := s.DeleteGrant(ctx, g3.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("second DeleteGrant: err = %v, want ErrNotFound", err)
	}
	if err := s.DeleteSandbox(ctx, a2.ID); err != nil {
		t.Errorf("DeleteSandbox without grants: %v", err)
	}

	// ClearMCPToken.
	if err := s.ClearMCPToken(ctx, a1.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.ClearMCPToken(ctx, a1.ID); err != nil {
		t.Errorf("second ClearMCPToken: %v", err)
	}
	sb, err := s.SandboxBySlug(ctx, pa.ID, "one")
	if err != nil || sb.MCPTokenHash != "" {
		t.Errorf("after ClearMCPToken: hash %q, %v", sb.MCPTokenHash, err)
	}
	if _, _, err := s.SandboxByTokenHash(ctx, hashOf("tok-one")); !errors.Is(err, ErrNotFound) {
		t.Errorf("old token still works: %v", err)
	}
	if err := s.ClearMCPToken(ctx, "nosuchsandbx"); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown sandbox: err = %v, want ErrNotFound", err)
	}
}
