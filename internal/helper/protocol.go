// Package helper holds the protocol, server and client of wsm-helper, the
// only component that runs as root (CLAUDE.md invariant 4, ADR 0004).
//
// The protocol is one JSON request per connection, answered by one JSON
// response. The client half-closes its side after the request. Requests are
// typed: there are no fields for paths, names or commands, only a request
// type and, where needed, a project UID from the fixed range. The server
// derives everything else (user name, home, socket directory) from the UID.
package helper

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"strings"
)

// DefaultSocket is where wsm-helper listens.
const DefaultSocket = "/run/wsm/helper.sock"

// MaxMessage caps a request or a response, in bytes.
const MaxMessage = 4096

// The project UID range (ADR 0004).
const (
	MinUID = 50000
	MaxUID = 59999
)

// Request types.
const (
	TypeCreateProjectUser = "create_project_user"
	TypeDeleteProjectUser = "delete_project_user"
	TypeSetupWSM0         = "setup_wsm0"
	TypeApplyBaseRules    = "apply_base_rules"
)

// Request is a typed request to wsm-helper.
type Request struct {
	Type string `json:"type"`
	UID  *int   `json:"uid,omitempty"`
}

// Response is the answer to one request.
type Response struct {
	OK    bool   `json:"ok"`
	Error string `json:"error,omitempty"`
}

// UserName returns the Linux user name for a project UID. The name is only
// ever derived from the UID, never taken from input.
func UserName(uid int) string { return fmt.Sprintf("wsm-p-%d", uid) }

// ValidUID reports whether uid is in the project range.
func ValidUID(uid int) bool { return uid >= MinUID && uid <= MaxUID }

// needsUID tells for each request type whether it takes a UID.
var needsUID = map[string]bool{
	TypeCreateProjectUser: true,
	TypeDeleteProjectUser: true,
	TypeSetupWSM0:         false,
	TypeApplyBaseRules:    false,
}

// Validate checks a request against its type.
func (r Request) Validate() error {
	want, ok := needsUID[r.Type]
	if !ok {
		return fmt.Errorf("unknown request type %q", r.Type)
	}
	switch {
	case want && r.UID == nil:
		return fmt.Errorf("%s: uid is required", r.Type)
	case want && !ValidUID(*r.UID):
		return fmt.Errorf("%s: uid %d is outside %d-%d", r.Type, *r.UID, MinUID, MaxUID)
	case !want && r.UID != nil:
		return fmt.Errorf("%s: takes no uid", r.Type)
	}
	return nil
}

// DecodeRequest strictly parses and validates one request: a single JSON
// object of at most MaxMessage bytes with only known fields.
func DecodeRequest(r io.Reader) (Request, error) {
	var req Request
	data, err := io.ReadAll(io.LimitReader(r, MaxMessage+1))
	if err != nil {
		return req, fmt.Errorf("read request: %w", err)
	}
	if len(data) > MaxMessage {
		return req, fmt.Errorf("request exceeds %d bytes", MaxMessage)
	}
	if err := decodeStrict(data, &req); err != nil {
		return req, fmt.Errorf("decode request: %w", err)
	}
	if err := req.Validate(); err != nil {
		return req, err
	}
	return req, nil
}

// decodeStrict decodes exactly one JSON object into v. Unknown fields are
// errors, and so are known fields in another case: encoding/json would
// otherwise accept "UID" for "uid".
func decodeStrict(data []byte, v any) error {
	keys, err := topLevelKeys(data)
	if err != nil {
		return err
	}
	allowed := jsonFieldNames(v)
	seen := make(map[string]bool, len(keys))
	for _, k := range keys {
		if !allowed[k] {
			return fmt.Errorf("unknown field %q", k)
		}
		if seen[k] {
			return fmt.Errorf("duplicate field %q", k)
		}
		seen[k] = true
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return err
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return errors.New("trailing data after the message")
	}
	return nil
}

// topLevelKeys returns the keys of the JSON object in data, in order and
// with duplicates, which encoding/json would silently merge.
func topLevelKeys(data []byte) ([]string, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	if d, ok := tok.(json.Delim); !ok || d != '{' {
		return nil, errors.New("message is not a JSON object")
	}
	var keys []string
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return nil, err
		}
		k, ok := tok.(string)
		if !ok {
			return nil, errors.New("object key is not a string")
		}
		keys = append(keys, k)
		var skip json.RawMessage
		if err := dec.Decode(&skip); err != nil {
			return nil, err
		}
	}
	if _, err := dec.Token(); err != nil {
		return nil, err
	}
	return keys, nil
}

// jsonFieldNames returns the JSON names of the fields of the struct v points to.
func jsonFieldNames(v any) map[string]bool {
	t := reflect.TypeOf(v).Elem()
	names := make(map[string]bool, t.NumField())
	for i := range t.NumField() {
		name, _, _ := strings.Cut(t.Field(i).Tag.Get("json"), ",")
		if name != "" && name != "-" {
			names[name] = true
		}
	}
	return names
}
