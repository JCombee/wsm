package sandbox

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"regexp"
	"strings"
	"testing"

	"github.com/JCombee/wsm/internal/db"
)

func TestSandboxGetsMCPToken(t *testing.T) {
	ctx := context.Background()
	m, _, lp := newManager(t)
	var logs []string
	m.Logf = func(format string, args ...any) { logs = append(logs, fmt.Sprintf(format, args...)) }
	if _, err := m.CreateProject(ctx, ProjectSpec{Slug: "app", ImageRef: "localhost/wsm-test:latest"}, userName); err != nil {
		t.Fatal(err)
	}

	tokens := map[string]bool{}
	for _, slug := range []string{"one", "two"} {
		sb, err := m.CreateSandbox(ctx, "app", slug)
		if err != nil {
			t.Fatal(err)
		}
		env, _ := lp.bodies["container"]["env"].(map[string]any)
		if got, want := env["WSM_MCP_URL"], "http://10.255.0.1:7443/mcp/"+sb.ID; got != want {
			t.Errorf("WSM_MCP_URL = %v, want %v", got, want)
		}
		token, _ := env["WSM_MCP_TOKEN"].(string)
		if !regexp.MustCompile(`^[A-Za-z0-9_-]{43}$`).MatchString(token) {
			t.Fatalf("WSM_MCP_TOKEN has the wrong form (length %d)", len(token))
		}
		if tokens[token] {
			t.Fatal("two sandboxes got the same token")
		}
		tokens[token] = true

		sum := sha256.Sum256([]byte(token))
		want := hex.EncodeToString(sum[:])
		if sb.MCPTokenHash != want {
			t.Error("returned sandbox does not carry the token's hash")
		}
		var stored string
		if err := m.Store.DB.QueryRowContext(ctx,
			`SELECT mcp_token_hash FROM sandboxes WHERE id = ?`, sb.ID).Scan(&stored); err != nil {
			t.Fatal(err)
		}
		if stored != want {
			t.Errorf("mcp_token_hash = %q, want the sha256 of the token", stored)
		}
		_, gotSB, err := m.Store.SandboxByTokenHash(ctx, want)
		if err != nil || gotSB.ID != sb.ID {
			t.Errorf("lookup by hash = %+v, %v", gotSB, err)
		}

		// The token is nowhere in the jobs or the logs.
		jobs, err := m.Store.JobsFor(ctx, sb.ID)
		if err != nil {
			t.Fatal(err)
		}
		for _, j := range jobs {
			if strings.Contains(j.Error, token) {
				t.Error("a job holds the token")
			}
		}
		for _, l := range logs {
			if strings.Contains(l, token) {
				t.Error("a log line holds the token")
			}
		}
	}
}

func TestWorkEnvWithoutToken(t *testing.T) {
	env := workEnv(db.Project{Slug: "p"}, db.Sandbox{ID: "abcdefgh2345", Slug: "s"}, "")
	for _, k := range []string{"WSM_MCP_URL", "WSM_MCP_TOKEN"} {
		if _, ok := env[k]; ok {
			t.Errorf("%s set without a token", k)
		}
	}
}
