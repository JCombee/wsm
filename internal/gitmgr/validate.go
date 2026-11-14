// Package gitmgr keeps a project's code on the host: its deploy key, a bare
// mirror of its origin and the bundles that carry code into a sandbox
// (plan sections 3.5 and 4, ADR 0009 and ADR 0010).
//
// git runs only on the mirror, which wsm alone controls, never in a
// directory an agent controls (CLAUDE.md invariant 3).
package gitmgr

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// MaxRepoURL is the longest repository URL wsm accepts.
const MaxRepoURL = 512

// MaxBranch is the longest base branch name wsm accepts.
const MaxBranch = 200

// DefaultBranch is the base branch when the owner gives none.
const DefaultBranch = "main"

var (
	userPattern = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9._-]{0,63}$`)
	hostPattern = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9.-]{0,251}[A-Za-z0-9])?$`)
	ipv6Pattern = regexp.MustCompile(`^\[[0-9A-Fa-f:.]{2,45}\]$`)
	pathPattern = regexp.MustCompile(`^[A-Za-z0-9._~+/-]+$`)
)

// ValidRepoURL checks a repository URL. Only SSH is allowed, in two forms:
// ssh://user@host[:port]/path and the scp-like user@host:path. Both need
// a user. Hosts, users and paths never start with a dash, so no part can
// pass for an option to git or ssh, and no part has spaces or control
// characters.
func ValidRepoURL(s string) error {
	if err := validRepoURL(s); err != nil {
		return fmt.Errorf("repository URL %q: %w", s, err)
	}
	return nil
}

func validRepoURL(s string) error {
	if len(s) == 0 || len(s) > MaxRepoURL {
		return fmt.Errorf("must be 1-%d bytes", MaxRepoURL)
	}
	var user, host, port, path string
	if rest, ok := strings.CutPrefix(s, "ssh://"); ok {
		var auth string
		var found bool
		auth, path, found = strings.Cut(rest, "/")
		if !found {
			return fmt.Errorf("has no path")
		}
		path = "/" + path
		user, host, found = strings.Cut(auth, "@")
		if !found {
			return fmt.Errorf("has no user; use ssh://user@host/path")
		}
		if strings.HasPrefix(host, "[") {
			if i := strings.Index(host, "]"); i >= 0 && i+1 < len(host) {
				host, port = host[:i+1], host[i+1:]
			}
		} else if h, p, ok := strings.Cut(host, ":"); ok {
			host, port = h, ":"+p
		}
		if port != "" {
			p, ok := strings.CutPrefix(port, ":")
			n, err := strconv.Atoi(p)
			if !ok || err != nil || n < 1 || n > 65535 || p != strconv.Itoa(n) {
				return fmt.Errorf("port %q is not 1-65535", port)
			}
		}
		if !hostPattern.MatchString(host) && !ipv6Pattern.MatchString(host) {
			return fmt.Errorf("host %q is not a host name or address", host)
		}
	} else {
		if strings.Contains(s, "://") || strings.Contains(s, "::") {
			return fmt.Errorf("only ssh:// URLs and user@host:path are allowed")
		}
		auth, p, found := strings.Cut(s, ":")
		if !found {
			return fmt.Errorf("is not ssh://user@host/path or user@host:path")
		}
		path = p
		user, host, found = strings.Cut(auth, "@")
		if !found {
			return fmt.Errorf("has no user; use user@host:path")
		}
		if !hostPattern.MatchString(host) {
			return fmt.Errorf("host %q is not a host name", host)
		}
	}
	if !userPattern.MatchString(user) {
		return fmt.Errorf("user %q is not valid", user)
	}
	if path == "" || path == "/" || strings.HasPrefix(path, "-") || !pathPattern.MatchString(path) {
		return fmt.Errorf("path %q is not valid", path)
	}
	for _, seg := range strings.Split(path, "/") {
		if seg == ".." {
			return fmt.Errorf("path %q has a .. segment", path)
		}
	}
	return nil
}

var branchPattern = regexp.MustCompile(`^[A-Za-z0-9._/-]+$`)

// ValidBranch checks a base branch name. It allows a strict subset of what
// git check-ref-format allows for refs/heads/<name>: letters, digits and
// . _ / -, without a leading dash, empty or dot-led components, "..", a
// trailing dot or a component ending in ".lock".
func ValidBranch(s string) error {
	bad := func(why string) error { return fmt.Errorf("branch %q: %s", s, why) }
	switch {
	case len(s) == 0 || len(s) > MaxBranch:
		return bad(fmt.Sprintf("must be 1-%d bytes", MaxBranch))
	case !branchPattern.MatchString(s):
		return bad("use letters, digits and . _ / -")
	case strings.HasPrefix(s, "-"):
		return bad("starts with a dash")
	case strings.Contains(s, ".."):
		return bad("contains ..")
	case strings.HasSuffix(s, "."):
		return bad("ends with a dot")
	case s == "HEAD":
		return bad("is HEAD")
	}
	for _, c := range strings.Split(s, "/") {
		if c == "" || strings.HasPrefix(c, ".") || strings.HasSuffix(c, ".lock") {
			return bad("has an empty, dot-led or .lock component")
		}
	}
	return nil
}
