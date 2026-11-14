// Command mcpspike is the throwaway program of spike 03-mcp
// (docs/plan/bouwplan-fase-1.md, sections 3.9, 3.10 and 5). It has three
// subcommands:
//
//	serve    the MCP server: Streamable HTTP, a bearer token per sandbox,
//	         three stub tools and a JSONL audit log
//	mockllm  a scripted mock model for Claude Code and Codex, so the agents
//	         make real MCP tool calls without real model credentials
//	probe    a small MCP client that checks the server from inside a sandbox
//
// Spike code; it never becomes part of the main module.
package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := run(ctx, os.Args[1:], os.Stdout, os.Stderr)
	stop()
	os.Exit(code)
}

func run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "usage: mcpspike serve|mockllm|probe [flags]")
		return 2
	}
	var err error
	switch args[0] {
	case "serve":
		err = runServe(ctx, args[1:], stderr)
	default:
		fmt.Fprintf(stderr, "mcpspike: unknown subcommand %q\n", args[0])
		return 2
	}
	if err != nil {
		fmt.Fprintf(stderr, "mcpspike %s: %v\n", args[0], err)
		return 1
	}
	return 0
}
