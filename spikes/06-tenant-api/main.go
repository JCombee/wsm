// Command tenantspike is spike 06 (S3 in docs/plan/plan-paperclip.md): a
// prototype of wsm's tenant API, the narrow API through which a project's
// control room manages that project's sandboxes.
//
//	tenantspike serve [-config /etc/s06/tenants.json]
//	tenantspike client <scenario> [flags]
//	tenantspike audit-verify <file>
//
// This is throwaway spike code with its own module. Its host setup, a server
// user in the groups wsm and wsm-admin, is a stand-in for the manager on a
// disposable VM only. See FINDINGS.md.
package main

import (
	"fmt"
	"os"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "serve":
		err = runServe(os.Args[2:])
	case "client":
		err = runClient(os.Args[2:])
	case "audit-verify":
		err = runAuditVerify(os.Args[2:])
	default:
		usage()
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "tenantspike:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: tenantspike serve [-config file] | client <scenario> [flags] | audit-verify <file>")
}

func runAuditVerify(args []string) error {
	if len(args) != 1 {
		return fmt.Errorf("audit-verify needs exactly one file")
	}
	f, err := os.Open(args[0])
	if err != nil {
		return err
	}
	defer f.Close()
	n, err := VerifyAudit(f)
	if err != nil {
		return err
	}
	fmt.Printf("ok: %d lines\n", n)
	return nil
}
