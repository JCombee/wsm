// Command wsm-helper is the only privileged part of wsm. It runs as root and
// serves typed requests from the manager over a unix socket (CLAUDE.md
// invariant 4, ADR 0004).
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log"
	"net/netip"
	"os"
	"os/signal"
	"os/user"
	"strconv"
	"syscall"

	"github.com/JCombee/wsm/internal/helper"
	"github.com/JCombee/wsm/internal/netpolicy"
	"github.com/JCombee/wsm/internal/sdsocket"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

const usage = "usage: wsm-helper version | serve [--socket PATH]"

// managerUser is the only user wsm-helper serves (CLAUDE.md invariant 4).
// It is fixed on purpose: no flag or setting can widen who may connect.
const managerUser = "wsm"

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := run(ctx, os.Args[1:], os.Stdout, os.Stderr)
	stop()
	os.Exit(code)
}

func run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) == 1 && (args[0] == "version" || args[0] == "--version") {
		fmt.Fprintf(stdout, "wsm-helper %s\n", version)
		return 0
	}
	if len(args) >= 1 && args[0] == "serve" {
		if err := serve(ctx, args[1:], stderr); err != nil {
			fmt.Fprintf(stderr, "wsm-helper: %v\n", err)
			return 1
		}
		return 0
	}
	fmt.Fprintln(stderr, usage)
	return 2
}

func serve(ctx context.Context, args []string, stderr io.Writer) error {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	fs.SetOutput(stderr)
	socket := fs.String("socket", helper.DefaultSocket, "unix socket to listen on, unless systemd passes one")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("unexpected arguments: %q", fs.Args())
	}
	if os.Geteuid() != 0 {
		return fmt.Errorf("must run as root")
	}
	u, err := user.Lookup(managerUser)
	if err != nil {
		return fmt.Errorf("look up manager user: %w", err)
	}
	uid, err := strconv.ParseUint(u.Uid, 10, 32)
	if err != nil || uid == 0 {
		return fmt.Errorf("manager user %s has unusable uid %q", managerUser, u.Uid)
	}
	logger := log.New(stderr, "", 0)
	ln, err := sdsocket.Listen(*socket, 0o600)
	if err != nil {
		return err
	}
	srv := &helper.Server{
		Ops: &helper.SystemOps{
			ManagerGroup: "wsm",
			WSM0Addr:     netip.MustParseAddr(netpolicy.DefaultWSM0Addr),
			MCPPort:      netpolicy.DefaultMCPPort,
			Logf:         logger.Printf,
		},
		AllowedUID: uint32(uid),
		Logf:       logger.Printf,
	}
	logger.Printf("wsm-helper %s serving uid %d on %s", version, uid, ln.Addr())
	return srv.Serve(ctx, ln)
}
