// Command wsm-helper is the only privileged part of wsm. It runs as root and
// serves typed requests from the manager over a unix socket.
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
		fmt.Fprintf(stdout, "wsm-helper %s\n", version)
		return 0
	}
	fmt.Fprintln(stderr, "usage: wsm-helper version")
	return 2
}
