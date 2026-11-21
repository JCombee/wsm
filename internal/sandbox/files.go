package sandbox

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/JCombee/wsm/internal/db"
	"github.com/JCombee/wsm/internal/fsproto"
)

// ErrFS is a failure that wsm-fs reported, such as a missing file.
var ErrFS = errors.New("file operation failed")

// FSError carries the code wsm-fs returned.
type FSError struct {
	Code string
	Msg  string
}

func (e *FSError) Error() string { return e.Msg }

// Unwrap lets errors.Is match ErrFS and, for not_found, db.ErrNotFound.
func (e *FSError) Unwrap() []error {
	if e.Code == fsproto.CodeNotFound {
		return []error{ErrFS, db.ErrNotFound}
	}
	if e.Code == fsproto.CodeInvalid || e.Code == fsproto.CodeTooLarge {
		return []error{ErrFS, ErrInvalid}
	}
	return []error{ErrFS}
}

// FS runs one wsm-fs operation in a running sandbox (plan section 3.7).
// The output is untrusted: it is capped by the exec limit, parsed strictly
// and checked.
func (m *Manager) FS(ctx context.Context, projectSlug, slug, op string, req fsproto.Request) (fsproto.Response, error) {
	var resp fsproto.Response
	if err := fsproto.ValidPath(req.Path); err != nil {
		return resp, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	if len(req.Content) > fsproto.MaxFile {
		return resp, fmt.Errorf("%w: content is larger than %d bytes", ErrInvalid, fsproto.MaxFile)
	}
	if m.Host.HelperDir == "" {
		return resp, fmt.Errorf("%w: the file helper is not configured", ErrState)
	}
	pc, ctr, err := m.ShellTarget(ctx, projectSlug, slug)
	if err != nil {
		return resp, err
	}
	defer pc.Close()
	in, err := json.Marshal(req)
	if err != nil {
		return resp, err
	}
	res, err := pc.RunInput(ctx, ctr, bytes.NewReader(in), fsproto.Binary, op)
	if err != nil {
		return resp, fmt.Errorf("wsm-fs %s: %w", op, err)
	}
	if res.ExitCode != 0 {
		// 126/127: the helper is missing, e.g. in a sandbox created before
		// the helper mount existed.
		return resp, fmt.Errorf("wsm-fs %s: exit code %d: %s", op, res.ExitCode, quote(res.Stderr))
	}
	dec := json.NewDecoder(bytes.NewReader([]byte(res.Stdout)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&resp); err != nil {
		return fsproto.Response{}, fmt.Errorf("wsm-fs %s: bad response: %w", op, err)
	}
	if dec.More() {
		return fsproto.Response{}, fmt.Errorf("wsm-fs %s: trailing data", op)
	}
	if err := resp.Check(op); err != nil {
		return fsproto.Response{}, fmt.Errorf("wsm-fs %s: bad response: %w", op, err)
	}
	if resp.Error != "" {
		return fsproto.Response{}, &FSError{Code: resp.Code, Msg: quote(resp.Error)}
	}
	return resp, nil
}
