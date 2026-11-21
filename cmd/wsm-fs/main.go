// Command wsm-fs is the file helper inside every sandbox (plan section
// 3.7). It is mounted read-only at /opt/wsm/bin and runs as the sandbox
// user through exec, so it can do nothing the agent cannot do. Usage:
//
//	wsm-fs list|stat|read|write
//
// It reads one JSON request from stdin and writes one JSON response to
// stdout (internal/fsproto). It exits non-zero only when it cannot answer.
package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"

	"github.com/JCombee/wsm/internal/fsproto"
)

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: wsm-fs list|stat|read|write < request.json")
		os.Exit(2)
	}
	resp := run(os.Args[1], os.Stdin)
	if err := json.NewEncoder(os.Stdout).Encode(resp); err != nil {
		os.Exit(1)
	}
}

func fail(code string, err error) fsproto.Response {
	return fsproto.Response{Code: code, Error: err.Error()}
}

func ioFail(err error) fsproto.Response {
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return fail(fsproto.CodeNotFound, err)
	case errors.Is(err, fs.ErrPermission):
		return fail(fsproto.CodeDenied, err)
	}
	return fail(fsproto.CodeIO, err)
}

func run(op string, stdin io.Reader) fsproto.Response {
	var req fsproto.Request
	// Base64 makes the content a third larger; allow some room for the rest.
	data, err := io.ReadAll(io.LimitReader(stdin, fsproto.MaxFile*2+fsproto.MaxPath+1024))
	if err != nil {
		return fail(fsproto.CodeIO, err)
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		return fail(fsproto.CodeInvalid, fmt.Errorf("request: %w", err))
	}
	if err := fsproto.ValidPath(req.Path); err != nil {
		return fail(fsproto.CodeInvalid, err)
	}
	switch op {
	case fsproto.OpList:
		return list(req.Path)
	case fsproto.OpStat:
		return stat(req.Path)
	case fsproto.OpRead:
		return read(req.Path)
	case fsproto.OpWrite:
		return write(req.Path, req.Content)
	}
	return fail(fsproto.CodeInvalid, fmt.Errorf("unknown operation %q", op))
}

func entry(name string, fi fs.FileInfo) fsproto.Entry {
	e := fsproto.Entry{Name: name, Size: fi.Size(), Mode: uint32(fi.Mode().Perm()), MTime: fi.ModTime().Unix()}
	switch {
	case fi.Mode().IsRegular():
		e.Type = fsproto.TypeFile
	case fi.IsDir():
		e.Type, e.Size = fsproto.TypeDir, 0
	case fi.Mode()&fs.ModeSymlink != 0:
		e.Type = fsproto.TypeSymlink
	default:
		e.Type, e.Size = fsproto.TypeOther, 0
	}
	return e
}

func list(p string) fsproto.Response {
	f, err := os.Open(p)
	if err != nil {
		return ioFail(err)
	}
	defer f.Close()
	des, err := f.ReadDir(fsproto.MaxEntries + 1)
	if err != nil && !errors.Is(err, io.EOF) {
		return ioFail(err)
	}
	resp := fsproto.Response{Entries: []fsproto.Entry{}}
	if len(des) > fsproto.MaxEntries {
		des, resp.Truncated = des[:fsproto.MaxEntries], true
	}
	for _, de := range des {
		fi, err := de.Info()
		if err != nil {
			continue
		}
		resp.Entries = append(resp.Entries, entry(de.Name(), fi))
	}
	sort.Slice(resp.Entries, func(i, j int) bool {
		a, b := resp.Entries[i], resp.Entries[j]
		if (a.Type == fsproto.TypeDir) != (b.Type == fsproto.TypeDir) {
			return a.Type == fsproto.TypeDir
		}
		return a.Name < b.Name
	})
	return resp
}

func stat(p string) fsproto.Response {
	fi, err := os.Lstat(p)
	if err != nil {
		return ioFail(err)
	}
	e := entry(filepath.Base(p), fi)
	return fsproto.Response{Entry: &e}
}

func read(p string) fsproto.Response {
	f, err := os.Open(p)
	if err != nil {
		return ioFail(err)
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return ioFail(err)
	}
	if !fi.Mode().IsRegular() {
		return fail(fsproto.CodeInvalid, fmt.Errorf("%s is not a regular file", p))
	}
	data, err := io.ReadAll(io.LimitReader(f, fsproto.MaxFile+1))
	if err != nil {
		return ioFail(err)
	}
	if len(data) > fsproto.MaxFile {
		return fail(fsproto.CodeTooLarge, fmt.Errorf("%s is larger than %d bytes", p, fsproto.MaxFile))
	}
	return fsproto.Response{Content: data}
}

// write replaces p atomically: a temporary file in the same directory,
// synced, then renamed over p. An existing file keeps its permissions.
func write(p string, content []byte) fsproto.Response {
	if len(content) > fsproto.MaxFile {
		return fail(fsproto.CodeTooLarge, fmt.Errorf("content is larger than %d bytes", fsproto.MaxFile))
	}
	mode := fs.FileMode(0o644)
	if fi, err := os.Stat(p); err == nil {
		if !fi.Mode().IsRegular() {
			return fail(fsproto.CodeInvalid, fmt.Errorf("%s is not a regular file", p))
		}
		mode = fi.Mode().Perm()
	}
	dir := filepath.Dir(p)
	tmp, err := os.CreateTemp(dir, ".wsm-fs-*")
	if err != nil {
		return ioFail(err)
	}
	ok := false
	defer func() {
		if !ok {
			os.Remove(tmp.Name())
		}
	}()
	if _, err := tmp.Write(content); err != nil {
		tmp.Close()
		return ioFail(err)
	}
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		return ioFail(err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return ioFail(err)
	}
	if err := tmp.Close(); err != nil {
		return ioFail(err)
	}
	if err := os.Rename(tmp.Name(), p); err != nil {
		return ioFail(err)
	}
	ok = true
	return stat(p)
}
