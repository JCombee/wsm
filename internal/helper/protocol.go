// Package helper holds the protocol, server and client of wsm-helper, the
// only component that runs as root (CLAUDE.md invariant 4, ADR 0004).
//
// The protocol is one JSON request per connection, answered by one JSON
// response. The client half-closes its side after the request. Requests are
// typed: there are no fields for paths, names, images or commands, only a
// request type and the fields that type needs: a project UID from the fixed
// range, a service from the block catalog, a tag, a service port and a root
// password, each validated. The server derives everything else (user name,
// home, image, unit text) from them.
package helper

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"strings"

	"github.com/JCombee/wsm/internal/blocks"
	"github.com/JCombee/wsm/internal/netpolicy"
	"github.com/JCombee/wsm/internal/projectuid"
)

// DefaultSocket is where wsm-helper listens.
const DefaultSocket = "/run/wsm/helper.sock"

// MaxMessage caps a request or a response, in bytes.
const MaxMessage = 4096

// Request types.
const (
	TypeCreateProjectUser = "create_project_user"
	TypeDeleteProjectUser = "delete_project_user"
	TypeSetupWSM0         = "setup_wsm0"
	TypeApplyBaseRules    = "apply_base_rules"

	// Shared service instances (ADR 0013).
	TypePullServiceImage      = "pull_service_image"
	TypeEnsureServiceInstance = "ensure_service_instance"
	TypeAllowService          = "allow_service"
	TypeRevokeService         = "revoke_service"
)

// The account shared service instances run under (ADR 0013). Its name and
// home are fixed; its UID comes from passwd.
const (
	ServiceUser = "wsm-svc"
	ServiceHome = "/var/lib/wsm-svc"
)

// PasswordLen is the length of a service root password: 32 random bytes
// as base64url without padding.
const PasswordLen = 43

// Request is a typed request to wsm-helper.
type Request struct {
	Type    string `json:"type"`
	UID     *int   `json:"uid,omitempty"`
	Service string `json:"service,omitempty"`
	Tag     string `json:"tag,omitempty"`
	Port    *int   `json:"port,omitempty"`
	// Password is the root password of a service instance. It is never
	// logged and never part of an error message.
	Password string `json:"password,omitempty"`
}

// Response is the answer to one request.
type Response struct {
	OK    bool   `json:"ok"`
	Error string `json:"error,omitempty"`
	// Code classifies some errors; see CodeNotFound.
	Code string `json:"code,omitempty"`
}

// CodeNotFound is the response code when the thing asked for does not
// exist, for example an image tag the registry does not know.
const CodeNotFound = "not_found"

// ErrNotFound is the error for a response with CodeNotFound. Ops return it
// (wrapped) to make the server answer with that code.
var ErrNotFound = errors.New("not found")

// UserName returns the Linux user name for a project UID. The name is only
// ever derived from the UID, never taken from input.
func UserName(uid int) string { return fmt.Sprintf("wsm-p-%d", uid) }

// Field names of a request besides "type".
const (
	fieldUID      = "uid"
	fieldService  = "service"
	fieldTag      = "tag"
	fieldPort     = "port"
	fieldPassword = "password"
)

// typeFields lists for each request type exactly the fields it takes. Every
// one of them is required, and every other field is an error.
var typeFields = map[string][]string{
	TypeCreateProjectUser:     {fieldUID},
	TypeDeleteProjectUser:     {fieldUID},
	TypeSetupWSM0:             nil,
	TypeApplyBaseRules:        nil,
	TypePullServiceImage:      {fieldService, fieldTag},
	TypeEnsureServiceInstance: {fieldService, fieldTag, fieldPort, fieldPassword},
	TypeAllowService:          {fieldUID, fieldPort},
	TypeRevokeService:         {fieldUID, fieldPort},
}

// present returns the fields of r that are set.
func (r Request) present() map[string]bool {
	return map[string]bool{
		fieldUID:      r.UID != nil,
		fieldService:  r.Service != "",
		fieldTag:      r.Tag != "",
		fieldPort:     r.Port != nil,
		fieldPassword: r.Password != "",
	}
}

// Validate checks a request against its type: exactly the fields of the
// type, each with a valid value.
func (r Request) Validate() error {
	fields, ok := typeFields[r.Type]
	if !ok {
		return fmt.Errorf("unknown request type %q", r.Type)
	}
	want := make(map[string]bool, len(fields))
	for _, f := range fields {
		want[f] = true
	}
	for _, f := range []string{fieldUID, fieldService, fieldTag, fieldPort, fieldPassword} {
		has := r.present()[f]
		switch {
		case want[f] && !has:
			return fmt.Errorf("%s: %s is required", r.Type, f)
		case !want[f] && has:
			return fmt.Errorf("%s: takes no %s", r.Type, f)
		}
	}
	if r.UID != nil && !projectuid.Valid(*r.UID) {
		return fmt.Errorf("%s: uid %d is outside %d-%d", r.Type, *r.UID, projectuid.Min, projectuid.Max)
	}
	if r.Service != "" {
		if _, err := blocks.LookupShared(r.Service); err != nil {
			return fmt.Errorf("%s: %w", r.Type, err)
		}
	}
	if r.Tag != "" {
		if err := blocks.ValidTag(r.Tag); err != nil {
			return fmt.Errorf("%s: %w", r.Type, err)
		}
	}
	if r.Port != nil && !netpolicy.ValidServicePort(*r.Port) {
		return fmt.Errorf("%s: port %d is outside %d-%d", r.Type, *r.Port,
			netpolicy.ServicePortMin, netpolicy.ServicePortMax)
	}
	if r.Password != "" && !ValidPassword(r.Password) {
		// The value itself never goes into the message.
		return fmt.Errorf("%s: password must be %d characters from [A-Za-z0-9_-]", r.Type, PasswordLen)
	}
	return nil
}

// ValidPassword reports whether p has the form of a service root password:
// PasswordLen characters of the base64url alphabet.
func ValidPassword(p string) bool {
	if len(p) != PasswordLen {
		return false
	}
	for i := 0; i < len(p); i++ {
		c := p[i]
		if !(c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '_' || c == '-') {
			return false
		}
	}
	return true
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
	keys, err := decodeStrictKeys(data, &req)
	if err != nil {
		return req, fmt.Errorf("decode request: %w", err)
	}
	// A field sent with an empty value ("service": "") decodes to the same
	// request as a missing one; it is still a field the type may not take.
	if fields, ok := typeFields[req.Type]; ok {
		allowed := map[string]bool{"type": true}
		for _, f := range fields {
			allowed[f] = true
		}
		for _, k := range keys {
			if !allowed[k] {
				return req, fmt.Errorf("%s: takes no %s", req.Type, k)
			}
		}
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
	_, err := decodeStrictKeys(data, v)
	return err
}

// decodeStrictKeys is decodeStrict that also returns the keys of the object.
func decodeStrictKeys(data []byte, v any) ([]string, error) {
	keys, err := topLevelKeys(data)
	if err != nil {
		return nil, err
	}
	allowed := jsonFieldNames(v)
	seen := make(map[string]bool, len(keys))
	for _, k := range keys {
		if !allowed[k] {
			return nil, fmt.Errorf("unknown field %q", k)
		}
		if seen[k] {
			return nil, fmt.Errorf("duplicate field %q", k)
		}
		seen[k] = true
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return nil, err
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, errors.New("trailing data after the message")
	}
	return keys, nil
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
