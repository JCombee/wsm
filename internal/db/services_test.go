package db

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/JCombee/wsm/internal/netpolicy"
)

func TestProjectBlocksStore(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	p := newProject("blocks")
	if err := s.CreateProject(ctx, p, userName); err != nil {
		t.Fatal(err)
	}
	other := newProject("other")
	if err := s.CreateProject(ctx, other, userName); err != nil {
		t.Fatal(err)
	}

	b := ProjectBlock{ProjectID: p.ID, BlockID: "mariadb", Profile: "dev", ConfigJSON: `{"tags":["11.8"]}`}
	if err := s.SetProjectBlock(ctx, b); err != nil {
		t.Fatalf("SetProjectBlock: %v", err)
	}
	// Upsert replaces profile and config.
	b.ConfigJSON = `{"tags":["11.8","12.3"]}`
	if err := s.SetProjectBlock(ctx, b); err != nil {
		t.Fatalf("SetProjectBlock again: %v", err)
	}
	if err := s.SetProjectBlock(ctx, ProjectBlock{ProjectID: other.ID, BlockID: "mariadb",
		Profile: "dev", ConfigJSON: `{"tags":["11.4"]}`}); err != nil {
		t.Fatal(err)
	}
	got, err := s.ProjectBlocks(ctx, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0] != b {
		t.Fatalf("ProjectBlocks = %+v, want [%+v]", got, b)
	}

	if err := s.SetProjectBlock(ctx, ProjectBlock{ProjectID: "nosuchprojct", BlockID: "mariadb",
		Profile: "dev", ConfigJSON: `{}`}); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown project: err = %v, want ErrNotFound", err)
	}
	if err := s.SetProjectBlock(ctx, ProjectBlock{ProjectID: p.ID, BlockID: "mariadb",
		Profile: "dev", ConfigJSON: `{"tags":`}); !errors.Is(err, ErrInvalid) {
		t.Errorf("invalid JSON: err = %v, want ErrInvalid", err)
	}

	if err := s.DeleteProjectBlock(ctx, p.ID, "mariadb"); err != nil {
		t.Fatalf("DeleteProjectBlock: %v", err)
	}
	if err := s.DeleteProjectBlock(ctx, p.ID, "mariadb"); !errors.Is(err, ErrNotFound) {
		t.Errorf("second delete: err = %v, want ErrNotFound", err)
	}
	if got, _ := s.ProjectBlocks(ctx, p.ID); len(got) != 0 {
		t.Errorf("after delete: %+v", got)
	}

	// Deleting the project removes its ceilings.
	if err := s.DeleteProject(ctx, other.ID); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := s.DB.QueryRow(`SELECT count(*) FROM project_blocks WHERE project_id = ?`, other.ID).Scan(&n); err != nil || n != 0 {
		t.Errorf("rows after project delete: %d, %v", n, err)
	}
}

func TestServiceInstancesStore(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	newInst := func(tag string) *ServiceInstance {
		id, err := NewID()
		if err != nil {
			t.Fatal(err)
		}
		return &ServiceInstance{ID: id, BlockID: "mariadb", Tag: tag, SecretEnc: []byte{0, 1, 2, 0xff}}
	}
	var made []*ServiceInstance
	for i, tag := range []string{"11.8", "12.3", "11.4"} {
		inst := newInst(tag)
		if err := s.CreateServiceInstance(ctx, inst); err != nil {
			t.Fatalf("CreateServiceInstance(%s): %v", tag, err)
		}
		if inst.Port != netpolicy.ServicePortMin+i {
			t.Errorf("%s: port %d, want %d", tag, inst.Port, netpolicy.ServicePortMin+i)
		}
		made = append(made, inst)
	}
	if err := s.CreateServiceInstance(ctx, newInst("11.8")); !errors.Is(err, ErrConflict) {
		t.Errorf("duplicate (block, tag): err = %v, want ErrConflict", err)
	}
	if err := s.CreateServiceInstance(ctx, &ServiceInstance{ID: "bad", BlockID: "mariadb", Tag: "13.0",
		SecretEnc: []byte{1}}); err == nil {
		t.Error("invalid id accepted")
	}
	if err := s.CreateServiceInstance(ctx, &ServiceInstance{ID: made[0].ID, BlockID: "mariadb", Tag: "13.0",
		SecretEnc: []byte{1}}); !errors.Is(err, ErrConflict) {
		t.Errorf("duplicate id: err = %v, want ErrConflict", err)
	}

	got, err := s.ServiceInstance(ctx, "mariadb", "12.3")
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != made[1].ID || got.Port != made[1].Port || !bytes.Equal(got.SecretEnc, made[1].SecretEnc) ||
		!got.CreatedAt.Equal(made[1].CreatedAt) {
		t.Errorf("ServiceInstance = %+v, want %+v", got, made[1])
	}
	if _, err := s.ServiceInstance(ctx, "mariadb", "99.9"); !errors.Is(err, ErrNotFound) {
		t.Errorf("missing instance: err = %v, want ErrNotFound", err)
	}
	all, err := s.ServiceInstances(ctx)
	if err != nil || len(all) != 3 {
		t.Fatalf("ServiceInstances = %d, %v", len(all), err)
	}
	for i, inst := range all {
		if inst.Port != netpolicy.ServicePortMin+i {
			t.Errorf("instance %d has port %d", i, inst.Port)
		}
	}

	// Fill the rest of the range directly, then the next one has no port.
	_, err = s.DB.Exec(`
		WITH RECURSIVE n(p) AS (SELECT ? UNION ALL SELECT p + 1 FROM n WHERE p < ?)
		INSERT INTO service_instances (id, block_id, tag, port, secret_enc, created_at)
		SELECT 'id' || p, 'mariadb', 't' || p, p, x'01', '2026-01-01T00:00:00.000000000Z' FROM n`,
		netpolicy.ServicePortMin+3, netpolicy.ServicePortMax)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.CreateServiceInstance(ctx, newInst("13.0")); err == nil {
		t.Error("CreateServiceInstance succeeded with a full port range")
	}
}

// TestServicePortCheckMatchesRange keeps the CHECK constraint on
// service_instances.port equal to the range in internal/netpolicy.
func TestServicePortCheckMatchesRange(t *testing.T) {
	s := openTest(t)
	for _, tt := range []struct {
		port int
		ok   bool
	}{
		{netpolicy.ServicePortMin - 1, false},
		{netpolicy.ServicePortMin, true},
		{netpolicy.ServicePortMax, true},
		{netpolicy.ServicePortMax + 1, false},
	} {
		_, err := s.DB.Exec(`INSERT INTO service_instances (id, block_id, tag, port, secret_enc, created_at)
			VALUES (?, 'mariadb', ?, ?, x'01', '2026-01-01T00:00:00.000000000Z')`,
			fmt.Sprint("id", tt.port), fmt.Sprint("t", tt.port), tt.port)
		if (err == nil) != tt.ok {
			t.Errorf("port %d: err = %v, want ok = %v", tt.port, err, tt.ok)
		}
	}
}
