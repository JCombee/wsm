// Package fsproto is the protocol between the manager and wsm-fs, the file
// helper in every sandbox (plan section 3.7). The manager runs
// "wsm-fs <op>" through exec, writes one Request as JSON to its stdin and
// reads one Response from its stdout. Everything wsm-fs returns is
// untrusted: the manager checks it with Response.Check.
package fsproto

import (
	"fmt"
	"path"
	"strings"
	"unicode/utf8"
)

// Path of the helper inside a sandbox, mounted read-only.
const (
	Dir    = "/opt/wsm/bin"
	Binary = Dir + "/wsm-fs"
)

// Operations.
const (
	OpList  = "list"
	OpStat  = "stat"
	OpRead  = "read"
	OpWrite = "write"
)

// Limits. A read's base64 content must fit in the manager's exec output
// limit of 1 MiB.
const (
	MaxFile    = 512 << 10
	MaxEntries = 2000
	MaxPath    = 4096
	MaxName    = 255
)

// Entry types.
const (
	TypeFile    = "file"
	TypeDir     = "dir"
	TypeSymlink = "symlink"
	TypeOther   = "other"
)

// Error codes.
const (
	CodeNotFound = "not_found"
	CodeTooLarge = "too_large"
	CodeInvalid  = "invalid"
	CodeDenied   = "denied"
	CodeIO       = "io"
)

// Request is the input of every operation. Content is only for write.
type Request struct {
	Path    string `json:"path"`
	Content []byte `json:"content,omitempty"`
}

// Entry describes one file system object.
type Entry struct {
	Name  string `json:"name"`
	Type  string `json:"type"`
	Size  int64  `json:"size"`
	Mode  uint32 `json:"mode"`
	MTime int64  `json:"mtime"` // Unix seconds
}

// Response is the output of every operation: Error, or the fields of the
// operation.
type Response struct {
	Error     string  `json:"error,omitempty"`
	Code      string  `json:"code,omitempty"`
	Entry     *Entry  `json:"entry,omitempty"`     // stat, write
	Entries   []Entry `json:"entries,omitempty"`   // list
	Truncated bool    `json:"truncated,omitempty"` // list
	Content   []byte  `json:"content,omitempty"`   // read
}

// ValidPath checks a path: absolute, clean, without NUL and not too long.
func ValidPath(p string) error {
	switch {
	case p == "" || len(p) > MaxPath:
		return fmt.Errorf("path must be 1-%d bytes", MaxPath)
	case !strings.HasPrefix(p, "/"):
		return fmt.Errorf("path must be absolute")
	case path.Clean(p) != p:
		return fmt.Errorf("path must be clean (no .., //, or trailing /)")
	case strings.ContainsRune(p, 0):
		return fmt.Errorf("path contains a NUL byte")
	case !utf8.ValidString(p):
		return fmt.Errorf("path is not valid UTF-8")
	}
	return nil
}

func (e *Entry) check() error {
	switch e.Type {
	case TypeFile, TypeDir, TypeSymlink, TypeOther:
	default:
		return fmt.Errorf("entry type %q", e.Type)
	}
	if len(e.Name) > MaxName*4 || strings.ContainsAny(e.Name, "/\x00") {
		return fmt.Errorf("entry name %q", e.Name)
	}
	if e.Size < 0 {
		return fmt.Errorf("entry size %d", e.Size)
	}
	return nil
}

// Check validates a response from wsm-fs for op.
func (r *Response) Check(op string) error {
	if r.Error != "" {
		if len(r.Error) > 1024 {
			r.Error = r.Error[:1024]
		}
		return nil
	}
	switch op {
	case OpList:
		if len(r.Entries) > MaxEntries {
			return fmt.Errorf("list returned %d entries", len(r.Entries))
		}
		for i := range r.Entries {
			if err := r.Entries[i].check(); err != nil {
				return err
			}
			if r.Entries[i].Name == "" {
				return fmt.Errorf("entry without a name")
			}
		}
	case OpStat, OpWrite:
		if r.Entry == nil {
			return fmt.Errorf("%s returned no entry", op)
		}
		return r.Entry.check()
	case OpRead:
		if len(r.Content) > MaxFile {
			return fmt.Errorf("read returned %d bytes", len(r.Content))
		}
	default:
		return fmt.Errorf("operation %q", op)
	}
	return nil
}
