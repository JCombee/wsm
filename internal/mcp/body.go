package mcp

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"unicode"
	"unicode/utf8"
)

// maxDepth bounds the nesting of a request body.
const maxDepth = 64

// Errors of the body rules.
var (
	errBatch     = errors.New("json-rpc batches are not accepted")
	errNotObject = errors.New("body is not a single json object")
	errDupKey    = errors.New("duplicate object key")
	errTooDeep   = errors.New("body is nested too deeply")
)

// rpcRequest is what the policy and the audit see of a JSON-RPC body. The
// fields are read with exact keys, as the SDK reads them.
type rpcRequest struct {
	Method string
	// ID is the raw "id" value, nil for a notification.
	ID json.RawMessage
	// ToolName and Arguments are params.name and params.arguments.
	ToolName  string
	Arguments json.RawMessage
}

// parseBody checks that body is exactly one JSON object without duplicate
// keys at any depth (keys that differ only in case count as duplicates)
// and returns its JSON-RPC fields.
func parseBody(body []byte) (rpcRequest, error) {
	trimmed := bytes.TrimLeft(body, " \t\r\n")
	switch {
	case len(trimmed) == 0:
		return rpcRequest{}, errNotObject
	case trimmed[0] == '[':
		return rpcRequest{}, errBatch
	case trimmed[0] != '{':
		return rpcRequest{}, errNotObject
	}
	if !utf8.Valid(body) {
		return rpcRequest{}, errors.New("body is not valid utf-8")
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	if err := checkValue(dec, 0); err != nil {
		return rpcRequest{}, err
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return rpcRequest{}, errNotObject
	}

	var top map[string]json.RawMessage
	if err := json.Unmarshal(body, &top); err != nil {
		return rpcRequest{}, fmt.Errorf("decode body: %w", err)
	}
	var req rpcRequest
	if raw, ok := top["method"]; ok {
		if err := json.Unmarshal(raw, &req.Method); err != nil {
			return rpcRequest{}, errors.New("method is not a string")
		}
	}
	if raw, ok := top["id"]; ok && string(raw) != "null" {
		req.ID = raw
	}
	if raw, ok := top["params"]; ok && string(raw) != "null" {
		var params map[string]json.RawMessage
		if err := json.Unmarshal(raw, &params); err != nil {
			return rpcRequest{}, errors.New("params is not an object")
		}
		if name, ok := params["name"]; ok {
			if err := json.Unmarshal(name, &req.ToolName); err != nil {
				return rpcRequest{}, errors.New("params.name is not a string")
			}
		}
		req.Arguments = params["arguments"]
	}
	return req, nil
}

// checkValue reads one JSON value from dec and rejects duplicate keys.
func checkValue(dec *json.Decoder, depth int) error {
	if depth > maxDepth {
		return errTooDeep
	}
	tok, err := dec.Token()
	if err != nil {
		return fmt.Errorf("decode body: %w", err)
	}
	switch tok {
	case json.Delim('{'):
		seen := map[string]bool{}
		for dec.More() {
			kt, err := dec.Token()
			if err != nil {
				return fmt.Errorf("decode body: %w", err)
			}
			key, ok := kt.(string)
			if !ok {
				return errNotObject
			}
			f := foldKey(key)
			if seen[f] {
				return errDupKey
			}
			seen[f] = true
			if err := checkValue(dec, depth+1); err != nil {
				return err
			}
		}
		if _, err := dec.Token(); err != nil {
			return fmt.Errorf("decode body: %w", err)
		}
	case json.Delim('['):
		for dec.More() {
			if err := checkValue(dec, depth+1); err != nil {
				return err
			}
		}
		if _, err := dec.Token(); err != nil {
			return fmt.Errorf("decode body: %w", err)
		}
	}
	return nil
}

// foldKey maps every rune to the smallest rune of its simple case-folding
// orbit, so two keys have the same foldKey exactly when strings.EqualFold
// holds for them.
func foldKey(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		m := r
		for f := unicode.SimpleFold(r); f != r; f = unicode.SimpleFold(f) {
			if f < m {
				m = f
			}
		}
		b.WriteRune(m)
	}
	return b.String()
}
