// Command wsm is the workspace manager: "wsm serve" runs the manager, the
// other subcommands are CLI clients of its API.
package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"sort"
	"syscall"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

// env is what every subcommand gets: arguments, streams and context. There
// is no global state; everything a command needs comes through here.
type env struct {
	ctx    context.Context
	args   []string
	stdin  io.Reader
	stdout io.Writer
	stderr io.Writer
}

type command struct {
	usage string
	run   func(e env) error
}

func commands() map[string]command {
	return map[string]command{
		"version": {"version", func(e env) error {
			fmt.Fprintf(e.stdout, "wsm %s\n", version)
			return nil
		}},
		"audit": {"audit verify [--db PATH]", runAudit},
	}
}

// usageError makes main print the usage and exit with code 2.
type usageError struct{ msg string }

func (u usageError) Error() string { return u.msg }

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := run(ctx, os.Args[1:], os.Stdin, os.Stdout, os.Stderr)
	stop()
	os.Exit(code)
}

func run(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	cmds := commands()
	if len(args) == 0 {
		printUsage(stderr, cmds)
		return 2
	}
	name := args[0]
	if name == "--version" {
		name = "version"
	}
	cmd, ok := cmds[name]
	if !ok {
		fmt.Fprintf(stderr, "wsm: unknown command %q\n", args[0])
		printUsage(stderr, cmds)
		return 2
	}
	err := cmd.run(env{ctx: ctx, args: args[1:], stdin: stdin, stdout: stdout, stderr: stderr})
	if err == nil {
		return 0
	}
	if u, ok := err.(usageError); ok {
		if u.msg != "" {
			fmt.Fprintf(stderr, "wsm: %s\n", u.msg)
		}
		fmt.Fprintf(stderr, "usage: wsm %s\n", cmd.usage)
		return 2
	}
	if c, ok := err.(exitCode); ok {
		return int(c)
	}
	fmt.Fprintf(stderr, "wsm: %v\n", err)
	return 1
}

// exitCode ends the command with a code and no further message.
type exitCode int

func (c exitCode) Error() string { return fmt.Sprintf("exit code %d", int(c)) }

func printUsage(w io.Writer, cmds map[string]command) {
	names := make([]string, 0, len(cmds))
	for n := range cmds {
		names = append(names, n)
	}
	sort.Strings(names)
	fmt.Fprintln(w, "usage:")
	for _, n := range names {
		fmt.Fprintf(w, "  wsm %s\n", cmds[n].usage)
	}
}
