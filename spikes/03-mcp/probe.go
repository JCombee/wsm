package main

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// runProbe checks the MCP server from inside a sandbox. The URL and token
// come from WSM_MCP_URL and WSM_MCP_TOKEN, never from flags. It prints one
// line per check: PROBE<TAB>name<TAB>PASS|FAIL<TAB>detail, plus INFO lines.
// The exit code is 0 when every check passed.
func runProbe(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("probe", flag.ContinueOnError)
	fs.SetOutput(stderr)
	otherURL := fs.String("other-url", "", "MCP URL of another sandbox, for the cross check")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	url, token := os.Getenv("WSM_MCP_URL"), os.Getenv("WSM_MCP_TOKEN")
	if url == "" || token == "" || *otherURL == "" {
		fmt.Fprintln(stderr, "probe: WSM_MCP_URL, WSM_MCP_TOKEN and -other-url are required")
		return 2
	}
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()

	failed := 0
	report := func(name string, ok bool, detail string) {
		status := "PASS"
		if !ok {
			status = "FAIL"
			failed++
		}
		fmt.Fprintf(stdout, "PROBE\t%s\t%s\t%s\n", name, status, oneLine(detail))
	}

	// 1. No token, a random token, and our token on another sandbox's path.
	code, err := rawInitialize(ctx, url, "")
	report("noauth", err == nil && code == http.StatusUnauthorized, fmt.Sprintf("no token: status %d %v", code, errText(err)))
	code, err = rawInitialize(ctx, url, "Bearer "+randomToken())
	report("badtoken", err == nil && code == http.StatusUnauthorized, fmt.Sprintf("random token: status %d %v", code, errText(err)))
	code, err = rawInitialize(ctx, *otherURL, "Bearer "+token)
	report("cross", err == nil && code == http.StatusForbidden, fmt.Sprintf("own token on other path: status %d %v", code, errText(err)))

	// 2. A real session with our token.
	client := mcp.NewClient(&mcp.Implementation{Name: "wsm-probe", Version: "0.0.1"}, nil)
	cs, err := client.Connect(ctx, &mcp.StreamableClientTransport{
		Endpoint:   url,
		HTTPClient: &http.Client{Transport: bearerRoundTripper{token: token}, Timeout: 30 * time.Second},
	}, nil)
	if err != nil {
		report("reach", false, "connect: "+err.Error())
		report("no-id-args", false, "no session")
		report("deny", false, "no session")
		report("identity", false, "no session")
		return 1
	}
	defer cs.Close()
	if ir := cs.InitializeResult(); ir != nil {
		fmt.Fprintf(stdout, "INFO\tprobe protocol %s, server %s\n", ir.ProtocolVersion, ir.ServerInfo.Name)
	}
	tools, err := cs.ListTools(ctx, nil)
	if err != nil {
		report("reach", false, "tools/list: "+err.Error())
	} else {
		var names []string
		for _, t := range tools.Tools {
			names = append(names, t.Name)
		}
		sort.Strings(names)
		report("reach", len(names) == 3, "initialize and tools/list ok: "+strings.Join(names, ","))
		bad := identityArguments(tools.Tools)
		report("no-id-args", len(bad) == 0 && len(names) > 0, "identity-like arguments: ["+strings.Join(bad, ",")+"]")
	}

	res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "service_enable", Arguments: map[string]any{"service": "mariadb"}})
	if err != nil {
		report("deny", false, "service_enable: "+err.Error())
	} else {
		text := toolText(res)
		report("deny", res.IsError && strings.Contains(text, "not available"), fmt.Sprintf("isError=%v: %s", res.IsError, text))
	}

	res, err = cs.CallTool(ctx, &mcp.CallToolParams{Name: "sandbox_info"})
	if err != nil {
		report("identity", false, "sandbox_info: "+err.Error())
	} else {
		text := toolText(res)
		want := "sandbox " + url[strings.LastIndex(url, "/")+1:]
		report("identity", !res.IsError && text == want, "sandbox_info: "+text)
	}
	if failed > 0 {
		return 1
	}
	return 0
}

// rawInitialize sends one initialize request and returns the HTTP status.
func rawInitialize(ctx context.Context, url, authz string) (int, error) {
	body := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"wsm-probe","version":"0"}}}`
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, strings.NewReader(body))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	if authz != "" {
		req.Header.Set("Authorization", authz)
	}
	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		return 0, err
	}
	io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
	resp.Body.Close()
	return resp.StatusCode, nil
}

// identityArguments lists tool arguments whose name mentions a project or
// sandbox: the identity must come only from the token.
func identityArguments(tools []*mcp.Tool) []string {
	var bad []string
	for _, t := range tools {
		b, err := json.Marshal(t.InputSchema)
		if err != nil {
			bad = append(bad, t.Name+":(unreadable schema)")
			continue
		}
		var schema struct {
			Properties map[string]json.RawMessage `json:"properties"`
		}
		if err := json.Unmarshal(b, &schema); err != nil {
			bad = append(bad, t.Name+":(unreadable schema)")
			continue
		}
		for p := range schema.Properties {
			l := strings.ToLower(p)
			if strings.Contains(l, "project") || strings.Contains(l, "sandbox") {
				bad = append(bad, t.Name+"."+p)
			}
		}
	}
	sort.Strings(bad)
	return bad
}

func toolText(r *mcp.CallToolResult) string {
	var parts []string
	for _, c := range r.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			parts = append(parts, tc.Text)
		}
	}
	return strings.Join(parts, " ")
}

type bearerRoundTripper struct{ token string }

func (b bearerRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+b.token)
	return http.DefaultTransport.RoundTrip(r)
}

// randomToken returns a token that matches no sandbox.
func randomToken() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

func errText(err error) string {
	if err == nil {
		return ""
	}
	return "(" + err.Error() + ")"
}

func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }
