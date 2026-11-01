// Command bindingsprobe checks whether the official Podman Go bindings build
// without cgo (tags remote and containers_image_openpgp) and can talk to a
// project user's socket. It is built only on the spike VM.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/containers/podman/v5/pkg/bindings"
	"github.com/containers/podman/v5/pkg/bindings/system"
)

func main() {
	socket := flag.String("socket", "", "path to the Podman API socket")
	flag.Parse()
	if *socket == "" {
		fmt.Fprintln(os.Stderr, "usage: bindingsprobe -socket PATH")
		os.Exit(2)
	}
	if err := run(*socket); err != nil {
		fmt.Fprintf(os.Stderr, "bindingsprobe: %v\n", err)
		os.Exit(1)
	}
}

func run(socket string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	conn, err := bindings.NewConnection(ctx, "unix://"+socket)
	if err != nil {
		return fmt.Errorf("connect: %w", err)
	}
	v, err := system.Version(conn, nil)
	if err != nil {
		return fmt.Errorf("version: %w", err)
	}
	if v.Server == nil {
		return fmt.Errorf("version: no server version in response")
	}
	fmt.Printf("server version %s, api %s\n", v.Server.Version, v.Server.APIVersion)
	return nil
}
