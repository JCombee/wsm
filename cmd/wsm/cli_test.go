package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/JCombee/wsm/internal/db"
)

func TestParseCPUs(t *testing.T) {
	tests := []struct {
		in   string
		want int64
		ok   bool
	}{
		{"1", 1000, true},
		{"1.5", 1500, true},
		{"0.25", 250, true},
		{"0", 0, false},
		{"-1", 0, false},
		{"NaN", 0, false},
		{"two", 0, false},
		{"1e9", 0, false},
	}
	for _, tt := range tests {
		got, err := parseCPUs(tt.in)
		if (err == nil) != tt.ok || got != tt.want {
			t.Errorf("parseCPUs(%q) = %d, %v", tt.in, got, err)
		}
	}
}

func TestParseBytes(t *testing.T) {
	tests := []struct {
		in   string
		want int64
		ok   bool
	}{
		{"1048576", 1 << 20, true},
		{"512m", 512 << 20, true},
		{"2G", 2 << 30, true},
		{"64k", 64 << 10, true},
		{"0", 0, false},
		{"-5m", 0, false},
		{"1.5g", 0, false},
		{"9999999999999g", 0, false},
		{"g", 0, false},
	}
	for _, tt := range tests {
		got, err := parseBytes(tt.in)
		if (err == nil) != tt.ok || got != tt.want {
			t.Errorf("parseBytes(%q) = %d, %v", tt.in, got, err)
		}
	}
}

func TestSplitTarget(t *testing.T) {
	tests := []struct {
		in, project, sandbox string
		ok                   bool
	}{
		{"app/main", "app", "main", true},
		{"app", "", "", false},
		{"/main", "", "", false},
		{"app/", "", "", false},
		{"app/main/x", "", "", false},
	}
	for _, tt := range tests {
		p, s, err := splitTarget(tt.in)
		if (err == nil) != tt.ok || p != tt.project || s != tt.sandbox {
			t.Errorf("splitTarget(%q) = %q, %q, %v", tt.in, p, s, err)
		}
	}
}

func TestCLIUsageErrors(t *testing.T) {
	for _, args := range [][]string{
		{"project"},
		{"project", "create", "--slug", "app"},
		{"project", "create", "--slug", "app", "--image", "img", "--memory", "lots"},
		{"project", "create", "--slug", "app", "--image", "img", "--containerfile", "Containerfile"},
		{"project", "image"},
		{"project", "image", "a", "b"},
		{"project", "delete"},
		{"sandbox", "start", "app"},
		{"sandbox", "reboot", "app/main"},
		{"shell"},
		{"serve", "extra"},
	} {
		var out, errOut bytes.Buffer
		if code := run(context.Background(), args, nil, &out, &errOut); code != 2 {
			t.Errorf("%q: exit code %d, want 2 (stderr %q)", args, code, errOut.String())
		}
	}
}

func TestReadContainerfile(t *testing.T) {
	dir := t.TempDir()
	write := func(name, content string) string {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	tests := []struct {
		name    string
		path    string
		want    string
		wantErr string
	}{
		{"ok", write("ok", "FROM x\n"), "FROM x\n", ""},
		{"at the limit", write("max", strings.Repeat("#", db.MaxContainerfile)), strings.Repeat("#", db.MaxContainerfile), ""},
		{"too large", write("big", strings.Repeat("#", db.MaxContainerfile+1)), "", "larger than"},
		{"missing", filepath.Join(dir, "missing"), "", "no such file"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := readContainerfile(tt.path)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("err = %v, want %q", err, tt.wantErr)
				}
				return
			}
			if err != nil || got != tt.want {
				t.Fatalf("readContainerfile = %d bytes, %v", len(got), err)
			}
		})
	}
}
