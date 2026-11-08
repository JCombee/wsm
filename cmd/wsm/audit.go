package main

import (
	"errors"
	"flag"
	"fmt"
	"io"

	"github.com/JCombee/wsm/internal/audit"
	"github.com/JCombee/wsm/internal/db"
)

// defaultDB is where the manager keeps its database.
const defaultDB = "/var/lib/wsm/wsm.db"

// runAudit implements "wsm audit verify". It reads the database file
// directly and read-only, so the check does not depend on the manager.
func runAudit(e env) error {
	if len(e.args) == 0 || e.args[0] != "verify" {
		return usageError{"unknown or missing audit subcommand"}
	}
	fs := flag.NewFlagSet("audit verify", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	path := fs.String("db", defaultDB, "path to the manager database")
	if err := fs.Parse(e.args[1:]); err != nil {
		return usageError{err.Error()}
	}
	if fs.NArg() != 0 {
		return usageError{"unexpected arguments"}
	}
	sqlDB, err := db.OpenReadOnly(e.ctx, *path)
	if err != nil {
		return err
	}
	defer sqlDB.Close()
	n, err := audit.New(sqlDB).Verify(e.ctx)
	var ve *audit.VerifyError
	if errors.As(err, &ve) {
		fmt.Fprintf(e.stdout, "FAIL: %v\n", ve)
		return exitCode(1)
	}
	if err != nil {
		return err
	}
	fmt.Fprintf(e.stdout, "OK: %d events, chain intact\n", n)
	return nil
}
