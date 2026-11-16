package blocks

import (
	"strings"
	"testing"
)

func TestCatalog(t *testing.T) {
	b, ok := Lookup("mariadb")
	if !ok {
		t.Fatal("mariadb is missing from the catalog")
	}
	if b.Kind != KindShared || b.Image != "docker.io/library/mariadb" || b.ContainerPort != 3306 {
		t.Errorf("mariadb = %+v", b)
	}
	if b.DefaultProfile() != "dev" || !b.HasProfile("dev") || b.HasProfile("prod") {
		t.Errorf("mariadb profiles = %v", b.Profiles)
	}
	if got := b.ImageRef("11.8"); got != "docker.io/library/mariadb:11.8" {
		t.Errorf("ImageRef = %q", got)
	}
	// Changing a returned block must not change the catalog.
	b.Profiles[0] = "changed"
	if again, _ := Lookup("mariadb"); again.DefaultProfile() != "dev" {
		t.Error("Lookup returns a shared Profiles slice")
	}
	r, ok := Lookup("redis")
	if !ok || r.Kind != KindPerSandbox || r.Image != "docker.io/library/redis" || r.ContainerPort != 6379 || r.MinTag != "7.2" || r.DefaultProfile() != "dev" {
		t.Errorf("redis = %+v, %v", r, ok)
	}
	if _, err := LookupShared("redis"); err == nil {
		t.Error("LookupShared(redis) succeeded for a per-sandbox block")
	}
	for _, id := range []string{"", "nosuchblock", "MariaDB", "mariadb "} {
		if _, ok := Lookup(id); ok {
			t.Errorf("Lookup(%q) found a block", id)
		}
		if _, err := LookupShared(id); err == nil {
			t.Errorf("LookupShared(%q) succeeded", id)
		}
	}
	if _, err := LookupShared("mariadb"); err != nil {
		t.Errorf("LookupShared(mariadb): %v", err)
	}
	for _, b := range All() {
		if b.ID == "" || len(b.Profiles) == 0 || b.ContainerPort <= 0 || strings.Contains(b.Image, ":") || ValidTagFormat(b.MinTag) != nil {
			t.Errorf("catalog entry %+v is incomplete", b)
		}
	}
}

func TestValidTagFormat(t *testing.T) {
	tests := []struct {
		tag string
		ok  bool
	}{
		{"10.11", true},
		{"11.4", true},
		{"11.8.9", true},
		{"12.3", true},
		{"13.0", true},
		{"999.999.999", true},
		{"10.6", true},
		{"8.10", true},
		{"7.1", true},
		{"latest", false},
		{"11", false},
		{"11.8.9.1", false},
		{"011.8", false},
		{"11.08", false},
		{"11.8-ubi", false},
		{"11.8;id", false},
		{"../11.8", false},
		{" 11.8", false},
		{"11.8\n", false},
		{"", false},
		{"1000.1", false},
		{"11.", false},
		{".11", false},
		{"11..8", false},
		{"+11.8", false},
		{"11.８", false},
		{strings.Repeat("1", 200), false},
		{"11." + strings.Repeat("1", 200), false},
	}
	for _, tt := range tests {
		err := ValidTagFormat(tt.tag)
		if (err == nil) != tt.ok {
			t.Errorf("ValidTagFormat(%q) = %v, want ok = %v", tt.tag, err, tt.ok)
		}
	}
	if got := Slug("11.8"); got != "11-8" {
		t.Errorf("Slug(11.8) = %q", got)
	}
	if got := Slug("11.8.9"); got != "11-8-9" {
		t.Errorf("Slug(11.8.9) = %q", got)
	}
	if got := Slug("8.10"); got != "8-10" {
		t.Errorf("Slug(8.10) = %q", got)
	}
}

func TestValidTagPerBlock(t *testing.T) {
	maria, _ := Lookup("mariadb")
	redis, _ := Lookup("redis")
	tests := []struct {
		blk Block
		tag string
		ok  bool
	}{
		{maria, "10.11", true},
		{maria, "11.8", true},
		{maria, "12.3", true},
		{maria, "10.6", false},
		{maria, "7.4", false},
		{maria, "10.10", false},
		{maria, "9.9", false},
		{redis, "7.2", true},
		{redis, "7.4", true},
		{redis, "8.10", true},
		{redis, "8.10.3", true},
		{redis, "6.2", false},
		{redis, "7.1", false},
	}
	for _, bad := range []string{"latest", "8", "08.1", "8.1;id", "8.1-alpine", "1234.1"} {
		tests = append(tests, struct {
			blk Block
			tag string
			ok  bool
		}{maria, bad, false}, struct {
			blk Block
			tag string
			ok  bool
		}{redis, bad, false})
	}
	for _, tt := range tests {
		err := tt.blk.ValidTag(tt.tag)
		if (err == nil) != tt.ok {
			t.Errorf("%s.ValidTag(%q) = %v, want ok = %v", tt.blk.ID, tt.tag, err, tt.ok)
		}
	}
	if p, err := ValidateCeiling("redis", "", []string{"8.10", "8.8"}); err != nil || p != "dev" {
		t.Errorf("ValidateCeiling(redis) = %q, %v", p, err)
	}
	if _, err := ValidateCeiling("redis", "", []string{"8.10", "6.2"}); err == nil {
		t.Error("ValidateCeiling(redis, 6.2) succeeded")
	}
	if _, err := ValidateCeiling("mariadb", "", []string{"8.10"}); err == nil {
		t.Error("ValidateCeiling(mariadb, 8.10) succeeded")
	}
}

func TestValidateCeiling(t *testing.T) {
	tests := []struct {
		name        string
		block, prof string
		tags        []string
		want        string // profile; empty means an error
	}{
		{"default profile", "mariadb", "", []string{"11.8"}, "dev"},
		{"explicit profile", "mariadb", "dev", []string{"11.8", "12.3"}, "dev"},
		{"eight tags", "mariadb", "", []string{"10.11", "11.0", "11.1", "11.2", "11.3", "11.4", "11.8", "12.3"}, "dev"},
		{"unknown block", "nosuchblock", "", []string{"11.8"}, ""},
		{"unknown profile", "mariadb", "prod", []string{"11.8"}, ""},
		{"no tags", "mariadb", "", nil, ""},
		{"nine tags", "mariadb", "", []string{"10.11", "11.0", "11.1", "11.2", "11.3", "11.4", "11.8", "12.3", "13.0"}, ""},
		{"duplicate", "mariadb", "", []string{"11.8", "11.8"}, ""},
		{"bad tag", "mariadb", "", []string{"11.8", "latest"}, ""},
	}
	for _, tt := range tests {
		got, err := ValidateCeiling(tt.block, tt.prof, tt.tags)
		if tt.want == "" {
			if err == nil {
				t.Errorf("%s: no error", tt.name)
			}
			continue
		}
		if err != nil || got != tt.want {
			t.Errorf("%s: = %q, %v; want %q", tt.name, got, err, tt.want)
		}
	}
}
