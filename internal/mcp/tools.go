package mcp

import (
	"context"
	"encoding/json"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// The tools. None takes a project or sandbox argument: the identity comes
// only from the token (CLAUDE.md invariant 2).
const (
	toolSandboxInfo       = "sandbox_info"
	toolServicesAvailable = "services_available"
	toolServiceEnable     = "service_enable"
)

// noInput is the input schema of a tool without arguments. It does not
// forbid extra properties: arguments are ignored, never used.
var noInput = json.RawMessage(`{"type":"object","properties":{}}`)

var serviceEnableInput = json.RawMessage(`{
	"type": "object",
	"properties": {"block": {"type": "string", "description": "the service block to enable, from services_available"}},
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
	}, sandboxInfo)
	srv.AddTool(&sdk.Tool{
		Name:        toolServicesAvailable,
		Description: "List the shared services this sandbox may enable.",
		InputSchema: noInput,
		Annotations: &sdk.ToolAnnotations{ReadOnlyHint: true, OpenWorldHint: &closedWorld},
	}, servicesAvailable)
	srv.AddTool(&sdk.Tool{
		Name:        toolServiceEnable,
		Description: "Enable a shared service for this sandbox. Not available yet.",
		InputSchema: serviceEnableInput,
		Annotations: &sdk.ToolAnnotations{OpenWorldHint: &closedWorld},
	}, serviceEnable)
	return srv
}

// SandboxInfo is the structured output of sandbox_info.
type SandboxInfo struct {
	Project    string   `json:"project"`
	Sandbox    string   `json:"sandbox"`
	Branch     string   `json:"branch"`
	PreviewURL string   `json:"preview_url"`
	Services   []string `json:"services"`
}

// ServicesAvailable is the structured output of services_available.
type ServicesAvailable struct {
	Services []string `json:"services"`
}

func sandboxInfo(_ context.Context, req *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
	if req.Extra == nil || req.Extra.TokenInfo == nil || req.Extra.TokenInfo.UserID == "" {
		return deniedResult("no sandbox identity"), nil
	}
	extra := req.Extra.TokenInfo.Extra
	str := func(k string) string { v, _ := extra[k].(string); return v }
	// The preview URL comes in M5, services in M3b.
	return jsonResult(SandboxInfo{
		Project:  str(extraProject),
		Sandbox:  str(extraSandbox),
		Branch:   str(extraBranch),
		Services: []string{},
	}), nil
}

func servicesAvailable(_ context.Context, req *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
	if req.Extra == nil || req.Extra.TokenInfo == nil || req.Extra.TokenInfo.UserID == "" {
		return deniedResult("no sandbox identity"), nil
	}
	// Blocks come in M3b.
	return jsonResult(ServicesAvailable{Services: []string{}}), nil
}

// serviceEnable refuses on its own, apart from the policy that already
// stops the call before it reaches the SDK.
func serviceEnable(context.Context, *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
	return deniedResult("service_enable is not available yet"), nil
}

func jsonResult(v any) *sdk.CallToolResult {
	b, err := json.Marshal(v)
	if err != nil {
		return &sdk.CallToolResult{IsError: true, Content: []sdk.Content{&sdk.TextContent{Text: "internal error"}}}
	}
	return &sdk.CallToolResult{
		Content:           []sdk.Content{&sdk.TextContent{Text: string(b)}},
		StructuredContent: v,
	}
}

func deniedResult(reason string) *sdk.CallToolResult {
	return &sdk.CallToolResult{IsError: true, Content: []sdk.Content{&sdk.TextContent{Text: "denied: " + reason}}}
}
