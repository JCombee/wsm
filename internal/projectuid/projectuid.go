// Package projectuid defines the UID range of project users (ADR 0004). The
// manager allocates project UIDs from it, wsm-helper accepts only UIDs in
// it, and the nftables base rules filter it (ADR 0001). All three use these
// constants, so they cannot drift apart. The CHECK constraint in the first
// database migration repeats the range; a test in internal/db keeps it equal.
package projectuid

// The range is inclusive.
const (
	Min = 50000
	Max = 59999
)

// Valid reports whether uid is a project UID.
func Valid(uid int) bool { return uid >= Min && uid <= Max }
