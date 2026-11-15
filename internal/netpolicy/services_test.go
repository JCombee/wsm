package netpolicy

import (
	"net/netip"
	"strings"
	"testing"
)

func TestServiceRuleScripts(t *testing.T) {
	wsm0 := netip.MustParseAddr(DefaultWSM0Addr)
	got, err := AllowServiceScript(wsm0, 50001, 20000)
	if err != nil || got != "add element inet wsm svc_allow4 { 50001 . 10.255.0.1 . 20000 }\n" {
		t.Errorf("AllowServiceScript = %q, %v", got, err)
	}
	got, err = RevokeServiceScript(wsm0, 50001, 20000)
	if err != nil || got != "destroy element inet wsm svc_allow4 { 50001 . 10.255.0.1 . 20000 }\n" {
		t.Errorf("RevokeServiceScript = %q, %v", got, err)
	}
	got, err = RevokeAllServicesScript(wsm0, 50001)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSuffix(got, "\n"), "\n")
	if len(lines) != ServicePortMax-ServicePortMin+1 {
		t.Errorf("RevokeAllServicesScript has %d lines", len(lines))
	}
	if lines[0] != "destroy element inet wsm svc_allow4 { 50001 . 10.255.0.1 . 20000 }" ||
		lines[len(lines)-1] != "destroy element inet wsm svc_allow4 { 50001 . 10.255.0.1 . 20999 }" {
		t.Errorf("first and last lines: %q, %q", lines[0], lines[len(lines)-1])
	}
	for _, l := range lines {
		if !strings.HasPrefix(l, "destroy element inet wsm svc_allow4 { 50001 . 10.255.0.1 . ") {
			t.Fatalf("unexpected line %q", l)
		}
	}

	for _, tt := range []struct{ uid, port int }{
		{49999, 20000}, {60000, 20000}, {0, 20000}, {-1, 20000},
		{50001, 19999}, {50001, 21000}, {50001, 7443}, {50001, 0},
	} {
		if s, err := AllowServiceScript(wsm0, tt.uid, tt.port); err == nil {
			t.Errorf("AllowServiceScript(%d, %d) = %q", tt.uid, tt.port, s)
		}
		if s, err := RevokeServiceScript(wsm0, tt.uid, tt.port); err == nil {
			t.Errorf("RevokeServiceScript(%d, %d) = %q", tt.uid, tt.port, s)
		}
	}
	for _, uid := range []int{49999, 60000} {
		if _, err := RevokeAllServicesScript(wsm0, uid); err == nil {
			t.Errorf("RevokeAllServicesScript(%d) succeeded", uid)
		}
	}
	if _, err := AllowServiceScript(netip.MustParseAddr("::1"), 50001, 20000); err == nil {
		t.Error("IPv6 wsm0 accepted")
	}
}
