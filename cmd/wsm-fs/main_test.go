package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/JCombee/wsm/internal/fsproto"
)

func req(t *testing.T, r fsproto.Request) *strings.Reader {
	t.Helper()
	b, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	return strings.NewReader(string(b))
}

func TestRun(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("hello"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "big"), make([]byte, fsproto.MaxFile+1), 0o644); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name, op string
		r        fsproto.Request
		code     string
		check    func(t *testing.T, resp fsproto.Response)
	}{
		{name: "list", op: "list", r: fsproto.Request{Path: dir}, check: func(t *testing.T, resp fsproto.Response) {
			if len(resp.Entries) != 3 || resp.Entries[0].Name != "sub" || resp.Entries[0].Type != fsproto.TypeDir {
				t.Errorf("entries = %+v", resp.Entries)
			}
		}},
		{name: "read", op: "read", r: fsproto.Request{Path: filepath.Join(dir, "a.txt")}, check: func(t *testing.T, resp fsproto.Response) {
			if string(resp.Content) != "hello" {
				t.Errorf("content = %q", resp.Content)
			}
		}},
		{name: "read too large", op: "read", r: fsproto.Request{Path: filepath.Join(dir, "big")}, code: fsproto.CodeTooLarge},
		{name: "read dir", op: "read", r: fsproto.Request{Path: dir}, code: fsproto.CodeInvalid},
		{name: "read missing", op: "read", r: fsproto.Request{Path: filepath.Join(dir, "nope")}, code: fsproto.CodeNotFound},
		{name: "relative path", op: "stat", r: fsproto.Request{Path: "a.txt"}, code: fsproto.CodeInvalid},
		{name: "unclean path", op: "stat", r: fsproto.Request{Path: dir + "/sub/../a.txt"}, code: fsproto.CodeInvalid},
		{name: "unknown op", op: "rm", r: fsproto.Request{Path: dir}, code: fsproto.CodeInvalid},
		{name: "write keeps mode", op: "write", r: fsproto.Request{Path: filepath.Join(dir, "a.txt"), Content: []byte("new")},
			check: func(t *testing.T, resp fsproto.Response) {
				if resp.Entry == nil || resp.Entry.Size != 3 || resp.Entry.Mode != 0o600 {
					t.Errorf("entry = %+v", resp.Entry)
				}
			}},
		{name: "write new", op: "write", r: fsproto.Request{Path: filepath.Join(dir, "sub", "n.txt"), Content: []byte("x")},
			check: func(t *testing.T, resp fsproto.Response) {
				if resp.Entry == nil || resp.Entry.Mode != 0o644 {
					t.Errorf("entry = %+v", resp.Entry)
				}
			}},
		{name: "write over dir", op: "write", r: fsproto.Request{Path: filepath.Join(dir, "sub"), Content: []byte("x")}, code: fsproto.CodeInvalid},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resp := run(tt.op, req(t, tt.r))
			if resp.Code != tt.code {
				t.Fatalf("code = %q (%s), want %q", resp.Code, resp.Error, tt.code)
			}
			if err := resp.Check(tt.op); tt.code == "" && err != nil {
				t.Fatalf("check: %v", err)
			}
			if tt.check != nil {
				tt.check(t, resp)
			}
		})
	}
	if left, _ := filepath.Glob(filepath.Join(dir, ".wsm-fs-*")); len(left) != 0 {
		t.Errorf("temporary files left: %v", left)
	}
}

func TestRunRejectsUnknownFields(t *testing.T) {
	resp := run("stat", strings.NewReader(`{"path":"/","x":1}`))
	if resp.Code != fsproto.CodeInvalid {
		t.Fatalf("code = %q", resp.Code)
	}
}
