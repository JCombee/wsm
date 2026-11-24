package main

import (
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// ExecSpec is the body of an exec request. The environment is a typed map:
// it travels to Podman as JSON and never appears in the argv of a host
// process.
type ExecSpec struct {
	Argv           []string          `json:"argv"`
	Env            map[string]string `json:"env,omitempty"`
	Workdir        string            `json:"workdir,omitempty"`
	TimeoutSeconds int               `json:"timeout_seconds"`
	Stdin          bool              `json:"stdin"`
	StdoutLimit    int64             `json:"stdout_limit,omitempty"`
	StderrLimit    int64             `json:"stderr_limit,omitempty"`
}

const (
	maxArgs          = 64
	maxArgvBytes     = 256 << 10
	maxEnv           = 256
	maxEnvBytes      = 1 << 20
	maxTimeout       = 4 * 3600
	defaultOutputCap = 64 << 20
	maxOutputCap     = 512 << 20
	maxSpecBody      = 2 << 20
	maxWorkdir       = 4096
)

var envNameRE = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// Validate checks the spec and fills in the default output limits.
func (s *ExecSpec) Validate() error {
	if len(s.Argv) == 0 || len(s.Argv) > maxArgs {
		return fmt.Errorf("argv needs 1 to %d elements", maxArgs)
	}
	total := 0
	for _, a := range s.Argv {
		if strings.IndexByte(a, 0) >= 0 {
			return errors.New("argv holds a NUL byte")
		}
		total += len(a)
	}
	if total > maxArgvBytes {
		return fmt.Errorf("argv is larger than %d bytes", maxArgvBytes)
	}
	if s.Argv[0] == "" || strings.HasPrefix(s.Argv[0], "-") {
		return errors.New("argv[0] must not be empty or start with '-'")
	}
	if len(s.Env) > maxEnv {
		return fmt.Errorf("env has more than %d names", maxEnv)
	}
	envBytes := 0
	for k, v := range s.Env {
		if !envNameRE.MatchString(k) {
			return errors.New("env holds an invalid name")
		}
		if strings.IndexByte(v, 0) >= 0 {
			return errors.New("env holds a NUL byte")
		}
		envBytes += len(k) + len(v) + 2
	}
	if envBytes > maxEnvBytes {
		return fmt.Errorf("env is larger than %d bytes", maxEnvBytes)
	}
	if s.Workdir != "" {
		if len(s.Workdir) > maxWorkdir || !filepath.IsAbs(s.Workdir) || filepath.Clean(s.Workdir) != s.Workdir ||
			strings.IndexByte(s.Workdir, 0) >= 0 {
			return errors.New("workdir must be a clean absolute path")
		}
	}
	if s.TimeoutSeconds < 1 || s.TimeoutSeconds > maxTimeout {
		return fmt.Errorf("timeout_seconds must be 1 to %d", maxTimeout)
	}
	for _, l := range []*int64{&s.StdoutLimit, &s.StderrLimit} {
		if *l == 0 {
			*l = defaultOutputCap
		}
		if *l < 1 || *l > maxOutputCap {
			return fmt.Errorf("output limits must be 1 to %d bytes", maxOutputCap)
		}
	}
	return nil
}

// auditInfo is what the audit keeps of the spec.
func (s *ExecSpec) auditInfo() *ExecAudit {
	names := make([]string, 0, len(s.Env))
	for k := range s.Env {
		names = append(names, k)
	}
	sort.Strings(names)
	argv0 := s.Argv[0]
	if len(argv0) > 256 {
		argv0 = argv0[:256]
	}
	return &ExecAudit{Argv0: argv0, Argc: len(s.Argv), EnvNames: names, TimeoutSeconds: s.TimeoutSeconds, Stdin: s.Stdin}
}

// envList turns the map into Podman's NAME=VALUE list.
func (s *ExecSpec) envList() []string {
	out := make([]string, 0, len(s.Env))
	for k, v := range s.Env {
		out = append(out, k+"="+v)
	}
	sort.Strings(out)
	return out
}
