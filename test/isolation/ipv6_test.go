//go:build isolation

package isolation

import (
	"context"
	"fmt"
	"net"
	"os"
	"testing"
)

const ipv6Dummy = "wsmiso6"

// TestIPv6Blocked checks the ip6 rules of table inet wsm: loopback, ULA
// (fc00::/7), link-local (fe80::/10) and other addresses of the host are
// blocked for project users. A dummy interface gives the host such
// addresses; a listener on all of them is the target.
func TestIPv6Blocked(t *testing.T) {
	ctx := ctxT(t)
	if _, err := os.Stat("/proc/sys/net/ipv6"); err != nil {
		t.Fatalf("IPv6 is not available on this host: %v", err)
	}
	run(ctx, "ip", "link", "del", ipv6Dummy)
	if _, err := run(ctx, "ip", "link", "add", ipv6Dummy, "type", "dummy"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { run(context.Background(), "ip", "link", "del", ipv6Dummy) })
	run(ctx, "sysctl", "-q", "-w", "net.ipv6.conf."+ipv6Dummy+".disable_ipv6=0")
	for _, a := range []string{"fd00:77::1/64", "2001:db8:77::1/64", "fe80::77/64"} {
		if _, err := run(ctx, "ip", "-6", "addr", "add", a, "dev", ipv6Dummy, "nodad"); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := run(ctx, "ip", "link", "set", ipv6Dummy, "up"); err != nil {
		t.Fatal(err)
	}
	const port = 18091
	listener(t, fmt.Sprintf("[::]:%d", port))

	for _, tc := range []struct {
		addr, counter string
	}{
		{"::1", "loopback_blocked"},
		{"fd00:77::1", "lan_blocked"},
		{"fe80::77%" + ipv6Dummy, "linklocal_blocked"},
		{"2001:db8:77::1", "host_blocked"},
	} {
		target := net.JoinHostPort(tc.addr, fmt.Sprint(port))
		rootConnects(t, target)
		for _, p := range []project{s.a, s.b} {
			blockedBy(t, tc.counter, p.slug+" user to "+target, func() (bool, string) {
				return probeAs(ctx, p.user, "tcp:"+target)
			})
		}
	}
}
