package main

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

// TestProbeAgainstServer runs the probe against a test server, as run.sh
// does from inside a sandbox.
func TestProbeAgainstServer(t *testing.T) {
	for _, stateless := range []bool{false, true} {
		audit := &memAudit{}
		srv := newTestServer(t, stateless, audit)
		t.Setenv("WSM_MCP_URL", srv.URL+"/mcp/"+idA)
		t.Setenv("WSM_MCP_TOKEN", tokenA)
		var out, errOut bytes.Buffer
		code := run(context.Background(), []string{"probe", "-other-url", srv.URL + "/mcp/" + idB}, &out, &errOut)
		if code != 0 {
			t.Fatalf("stateless=%v: probe exit %d\n%s%s", stateless, code, out.String(), errOut.String())
		}
		for _, name := range []string{"noauth", "badtoken", "cross", "reach", "no-id-args", "deny", "identity"} {
			if !strings.Contains(out.String(), "PROBE\t"+name+"\tPASS\t") {
				t.Errorf("stateless=%v: no PASS for %s:\n%s", stateless, name, out.String())
			}
		}
		if strings.Contains(out.String()+audit.rawString(), tokenA) {
			t.Error("token in probe output or audit log")
		}
	}
}
