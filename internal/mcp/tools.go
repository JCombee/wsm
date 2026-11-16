package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/JCombee/wsm/internal/blocks"
	"github.com/JCombee/wsm/internal/broker"
)

// The tools. None takes a project or sandbox argument: the identity comes
// only from the token (CLAUDE.md invariant 2).
const (
	toolSandboxInfo       = "sandbox_info"
	toolServicesAvailable = "services_available"
	toolServiceEnable     = "service_enable"
)

// Services is the part of the broker the tools use. Every method takes
// the sandbox ID from the token, never from the arguments.
type Services interface {
	Available(ctx context.Context, sandboxID string) ([]broker.Offer, error)
	Enabled(ctx context.Context, sandboxID string) ([]broker.Enabled, error)
	Enable(ctx context.Context, sandboxID, blockID, tag string) (broker.Credentials, error)
}

// noInput is the input schema of a tool without arguments. It does not
// forbid extra properties: arguments are ignored, never used.
var noInput = json.RawMessage(`{"type":"object","properties":{}}`)

var serviceEnableInput = json.RawMessage(`{
	"type": "object",
	"properties": {
		"block": {"type": "string", "description": "the service block to enable, from services_available"},
		"tag": {"type": "string", "description": "the version to use, one of the block's tags; default: its default_tag"}
	},
	"required": ["block"],
	"additionalProperties": false
}`)

// newMCPServer returns an SDK server with the tools. The handler makes one
// per request; the server is stateless.
func (s *Server) newMCPServer() *sdk.Server {
	version := s.Version
	if version == "" {
		version = "dev"
	}
	srv := sdk.NewServer(&sdk.Implementation{Name: "wsm", Version: version}, nil)
	closedWorld := false
	srv.AddTool(&sdk.Tool{
		Name:        toolSandboxInfo,
		Description: "Show the project, sandbox and branch this sandbox is, and its preview URL and enabled services.",
		InputSchema: noInput,
		Annotations: &sdk.ToolAnnotations{ReadOnlyHint: true, OpenWorldHint: &closedWorld},
	}, s.sandboxInfo)
	srv.AddTool(&sdk.Tool{
		Name:        toolServicesAvailable,
		Description: "List the shared services this sandbox may enable, with their tags (versions).",
		InputSchema: noInput,
		Annotations: &sdk.ToolAnnotations{ReadOnlyHint: true, OpenWorldHint: &closedWorld},
	}, s.servicesAvailable)
	srv.AddTool(&sdk.Tool{
		Name: toolServiceEnable,
		Description: "Enable a shared service for this sandbox and get its connection details: " +
			"an own account and database. Calling it again returns the same details.",
		InputSchema: serviceEnableInput,
		Annotations: &sdk.ToolAnnotations{OpenWorldHint: &closedWorld},
	}, s.serviceEnable)
	return srv
}

// ServiceInfo is an enabled service in sandbox_info, without password.
type ServiceInfo struct {
	Block    string `json:"block"`
	Tag      string `json:"tag"`
	Host     string `json:"host"`
	Port     int    `json:"port"`
	User     string `json:"user"`
	Database string `json:"database"`
}

// SandboxInfo is the structured output of sandbox_info.
type SandboxInfo struct {
	Project    string        `json:"project"`
	Sandbox    string        `json:"sandbox"`
	Branch     string        `json:"branch"`
	PreviewURL string        `json:"preview_url"`
	Services   []ServiceInfo `json:"services"`
}

// ServiceOffer is one block in services_available.
type ServiceOffer struct {
	Block       string   `json:"block"`
	Profile     string   `json:"profile"`
	Tags        []string `json:"tags"`
	DefaultTag  string   `json:"default_tag"`
	EnabledTags []string `json:"enabled_tags"`
}

// ServicesAvailable is the structured output of services_available.
type ServicesAvailable struct {
	Services []ServiceOffer `json:"services"`
}

// ServiceCredentials is the structured output of service_enable.
type ServiceCredentials struct {
	Block    string `json:"block"`
	Tag      string `json:"tag"`
	Host     string `json:"host"`
	Port     int    `json:"port"`
	User     string `json:"user"`
	Password string `json:"password"`
	Database string `json:"database"`
}

// sandboxID returns the sandbox of the token, or "".
func sandboxID(req *sdk.CallToolRequest) string {
	if req == nil || req.Extra == nil || req.Extra.TokenInfo == nil {
		return ""
	}
	return req.Extra.TokenInfo.UserID
}

func (s *Server) sandboxInfo(ctx context.Context, req *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
	id := sandboxID(req)
	if id == "" {
		return deniedResult("no sandbox identity"), nil
	}
	extra := req.Extra.TokenInfo.Extra
	str := func(k string) string { v, _ := extra[k].(string); return v }
	services := []ServiceInfo{}
	if s.Services != nil {
		en, err := s.Services.Enabled(ctx, id)
		if err != nil {
			s.logf("mcp: sandbox_info for %s: %v", id, err)
			return errorResult("internal error"), nil
		}
		for _, e := range en {
			services = append(services, ServiceInfo{Block: e.Block, Tag: e.Tag, Host: e.Host, Port: e.Port,
				User: e.User, Database: e.Database})
		}
	}
	// The preview URL comes in M5.
	return jsonResult(SandboxInfo{
		Project:  str(extraProject),
		Sandbox:  str(extraSandbox),
		Branch:   str(extraBranch),
		Services: services,
	}), nil
}

func (s *Server) servicesAvailable(ctx context.Context, req *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
	id := sandboxID(req)
	if id == "" {
		return deniedResult("no sandbox identity"), nil
	}
	out := ServicesAvailable{Services: []ServiceOffer{}}
	if s.Services != nil {
		offers, err := s.Services.Available(ctx, id)
		if err != nil {
			s.logf("mcp: services_available for %s: %v", id, err)
			return errorResult("internal error"), nil
		}
		for _, o := range offers {
			out.Services = append(out.Services, ServiceOffer{Block: o.Block, Profile: o.Profile,
				Tags: nonNil(o.Tags), DefaultTag: o.DefaultTag, EnabledTags: nonNil(o.EnabledTags)})
		}
	}
	return jsonResult(out), nil
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

var blockPattern = regexp.MustCompile(`^[a-z][a-z0-9-]{0,31}$`)

// enableArgs are the arguments of service_enable.
type enableArgs struct {
	Block string
	Tag   string
}

// decodeEnableArgs parses the arguments of service_enable strictly: one
// JSON object with exactly the keys "block" and optionally "tag", both
// strings, matched case-sensitively. It returns a reason on failure.
func decodeEnableArgs(raw json.RawMessage) (enableArgs, string) {
	var fields map[string]json.RawMessage
	dec := json.NewDecoder(bytes.NewReader(raw))
	if err := dec.Decode(&fields); err != nil || fields == nil {
		return enableArgs{}, "arguments must be an object with block and optionally tag"
	}
	if dec.More() {
		return enableArgs{}, "arguments must be one object"
	}
	var a enableArgs
	for k, v := range fields {
		var dst *string
		switch k {
		case "block":
			dst = &a.Block
		case "tag":
			dst = &a.Tag
		default:
			return enableArgs{}, "unknown argument (only block and tag are accepted)"
		}
		if err := json.Unmarshal(v, dst); err != nil {
			return enableArgs{}, k + " must be a string"
		}
	}
	if _, ok := fields["block"]; !ok {
		return enableArgs{}, "block is required"
	}
	if !blockPattern.MatchString(a.Block) {
		return enableArgs{}, "invalid block name"
	}
	if _, ok := fields["tag"]; ok {
		if err := blocks.ValidTagFormat(a.Tag); err != nil {
			return enableArgs{}, "invalid tag"
		}
	}
	return a, ""
}

// serviceEnable grants the sandbox of the token an account on a shared
// service. Its outcome goes into the request's audit event; the password
// is only ever in the tool result.
func (s *Server) serviceEnable(ctx context.Context, req *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
	rec := recordFrom(ctx)
	if rec == nil {
		rec = &record{}
	}
	id := sandboxID(req)
	if id == "" {
		rec.deny("no sandbox identity")
		return deniedResult("no sandbox identity"), nil
	}
	if s.Services == nil {
		rec.deny("services are not configured")
		return deniedResult("services are not configured"), nil
	}
	var raw json.RawMessage
	if req.Params != nil {
		raw = req.Params.Arguments
	}
	args, reason := decodeEnableArgs(raw)
	if reason != "" {
		rec.deny(reason)
		return deniedResult(reason), nil
	}
	c, err := s.Services.Enable(ctx, id, args.Block, args.Tag)
	switch {
	case errors.Is(err, broker.ErrDenied):
		reason := strings.TrimPrefix(err.Error(), broker.ErrDenied.Error()+": ")
		rec.deny(reason)
		return deniedResult(reason), nil
	case errors.Is(err, broker.ErrUnavailable):
		s.logf("mcp: service_enable %s for %s: %v", args.Block, id, err)
		rec.fail("service not available")
		return errorResult("service not available, try again later"), nil
	case err != nil:
		s.logf("mcp: service_enable %s for %s: %v", args.Block, id, err)
		rec.fail("internal error")
		return errorResult("internal error"), nil
	}
	rec.detail = fmt.Sprintf("granted %s %s", c.Block, c.Tag)
	return jsonResult(ServiceCredentials{Block: c.Block, Tag: c.Tag, Host: c.Host, Port: c.Port,
		User: c.User, Password: c.Password, Database: c.Database}), nil
}

func jsonResult(v any) *sdk.CallToolResult {
	b, err := json.Marshal(v)
	if err != nil {
		return errorResult("internal error")
	}
	return &sdk.CallToolResult{
		Content:           []sdk.Content{&sdk.TextContent{Text: string(b)}},
		StructuredContent: v,
	}
}

func deniedResult(reason string) *sdk.CallToolResult {
	return &sdk.CallToolResult{IsError: true, Content: []sdk.Content{&sdk.TextContent{Text: "denied: " + reason}}}
}

func errorResult(msg string) *sdk.CallToolResult {
	return &sdk.CallToolResult{IsError: true, Content: []sdk.Content{&sdk.TextContent{Text: "error: " + msg}}}
}
