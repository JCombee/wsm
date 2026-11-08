package netpolicy

import (
	"flag"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

var update = flag.Bool("update", false, "rewrite golden files")

func TestBaseGolden(t *testing.T) {
	resolvers := []netip.Addr{
		netip.MustParseAddr("192.0.2.53"),
		netip.MustParseAddr("127.0.0.53"),
		netip.MustParseAddr("2001:db8::53"),
		netip.MustParseAddr("192.0.2.53"), // duplicate
	}
	got, err := Base(DefaultConfig(resolvers))
	if err != nil {
		t.Fatal(err)
	}
	golden := filepath.Join("testdata", "base.nft")
	if *update {
		if err := os.WriteFile(golden, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(golden)
	if err != nil {
		t.Fatal(err)
	}
	if got != string(want) {
		t.Errorf("Base() differs from %s; run go test -update and review the diff.\n%s", golden, got)
	}
}

func TestBaseNoResolvers(t *testing.T) {
	got, err := Base(DefaultConfig(nil))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(got, "elements") {
		t.Errorf("empty sets have elements:\n%s", got)
	}
}

// The order of the rules is the security property: allow rules must come
// before the reject rules, and the fib rule must come after the allows.
func TestBaseRuleOrder(t *testing.T) {
	got, err := Base(DefaultConfig(nil))
	if err != nil {
		t.Fatal(err)
	}
	order := []string{
		"meta skuid != 50000-59999 accept",
		"ct state established,related accept",
		"counter name dns_allowed accept",
		"counter name mcp_allowed accept",
		"counter name svc_allowed accept",
		"counter name loopback_blocked reject",
		"counter name wsm0_blocked reject",
		"counter name lan_blocked reject",
		"counter name linklocal_blocked reject",
		"fib daddr type local counter name host_blocked reject",
	}
	pos := -1
	for _, s := range order {
		i := strings.Index(got, s)
		if i < 0 {
			t.Fatalf("missing %q", s)
		}
		if i < pos {
			t.Errorf("%q comes too early", s)
		}
		pos = i
	}
}

func TestBaseRejectsInvalidConfig(t *testing.T) {
	tests := map[string]func(c *Config){
		"uid range below 1000": func(c *Config) { c.UIDMin = 0 },
		"inverted uid range":   func(c *Config) { c.UIDMax = c.UIDMin - 1 },
		"ipv6 wsm0":            func(c *Config) { c.WSM0Addr = netip.MustParseAddr("fd00::1") },
		"no wsm0":              func(c *Config) { c.WSM0Addr = netip.Addr{} },
		"mcp port 0":           func(c *Config) { c.MCPPort = 0 },
		"invalid resolver":     func(c *Config) { c.Resolvers = []netip.Addr{{}} },
		"resolver with zone":   func(c *Config) { c.Resolvers = []netip.Addr{netip.MustParseAddr("fe80::1%eth0")} },
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			c := DefaultConfig(nil)
			mutate(&c)
			if _, err := Base(c); err == nil {
				t.Error("Base accepted the config")
			}
		})
	}
}

func TestParseResolvConf(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		want    []string
		wantErr bool
	}{
		{"typical", "# comment\nsearch example.org\nnameserver 192.0.2.1\nnameserver 2001:db8::1\noptions edns0\n",
			[]string{"192.0.2.1", "2001:db8::1"}, false},
		{"zone dropped", "nameserver fe80::1%enp1s0\n", []string{"fe80::1"}, false},
		{"mapped v4", "nameserver ::ffff:192.0.2.9\n", []string{"192.0.2.9"}, false},
		{"empty", "", nil, false},
		{"bare nameserver ignored", "nameserver\n", nil, false},
		{"bad address", "nameserver not-an-ip\n", nil, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParseResolvConf(strings.NewReader(tc.in))
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v", err)
			}
			var s []string
			for _, a := range got {
				s = append(s, a.String())
			}
			if strings.Join(s, ",") != strings.Join(tc.want, ",") {
				t.Errorf("got %v, want %v", s, tc.want)
			}
		})
	}
}
