// Command wsm is the workspace manager: "wsm serve" runs the manager, every
// other subcommand is a CLI client of its API.
package main

import (
	"fmt"
	"io"
	"os"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 1 && (args[0] == "version" || args[0] == "--version") {
		fmt.Fprintf(stdout, "wsm %s\n", version)
		return 0
	}
	fmt.Fprintln(stderr, "usage: wsm version")
	return 2
}
