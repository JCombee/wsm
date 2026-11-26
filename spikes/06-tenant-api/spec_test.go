package main

import (
	"slices"
	"strings"
	"testing"
)

func TestExecSpecValidate(t *testing.T) {
	ok := func() ExecSpec {
		return ExecSpec{Argv: []string{"sh", "-c", "true"}, Env: map[string]string{"A": "x\ny=z"}, TimeoutSeconds: 10}
	}
	tests := []struct {
		name   string
		change func(*ExecSpec)
		want   string
	}{
		{"valid", func(*ExecSpec) {}, ""},
		{"empty env value", func(s *ExecSpec) { s.Env["EMPTY"] = "" }, ""},
		{"no argv", func(s *ExecSpec) { s.Argv = nil }, "argv"},
		{"65 args", func(s *ExecSpec) { s.Argv = make([]string, 65); s.Argv[0] = "x" }, "argv"},
		{"NUL in argv", func(s *ExecSpec) { s.Argv[2] = "a\x00b" }, "NUL"},
		{"dash argv0", func(s *ExecSpec) { s.Argv[0] = "-x" }, "argv[0]"},
		{"empty argv0", func(s *ExecSpec) { s.Argv[0] = "" }, "argv[0]"},
		{"big argv", func(s *ExecSpec) { s.Argv[2] = strings.Repeat("a", maxArgvBytes) }, "argv"},
		{"bad env name", func(s *ExecSpec) { s.Env["1A"] = "x" }, "name"},
		{"env name with =", func(s *ExecSpec) { s.Env["A=B"] = "x" }, "name"},
		{"NUL in env", func(s *ExecSpec) { s.Env["B"] = "\x00" }, "NUL"},
		{"relative workdir", func(s *ExecSpec) { s.Workdir = "work" }, "workdir"},
		{"unclean workdir", func(s *ExecSpec) { s.Workdir = "/a/../b" }, "workdir"},
		{"no timeout", func(s *ExecSpec) { s.TimeoutSeconds = 0 }, "timeout"},
		{"timeout above 4 h", func(s *ExecSpec) { s.TimeoutSeconds = maxTimeout + 1 }, "timeout"},
		{"output limit too high", func(s *ExecSpec) { s.StdoutLimit = maxOutputCap + 1 }, "limit"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := ok()
			tt.change(&s)
			err := s.Validate()
			if tt.want == "" {
				if err != nil {
					t.Fatalf("Validate: %v", err)
				}
				if s.StdoutLimit != defaultOutputCap || s.StderrLimit != defaultOutputCap {
					t.Fatalf("default limits not set: %d %d", s.StdoutLimit, s.StderrLimit)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("Validate = %v, want an error about %q", err, tt.want)
			}
		})
	}
}

func TestAuditInfoHasNoValues(t *testing.T) {
	s := ExecSpec{Argv: []string{"node", "--token=SECRET1"}, Env: map[string]string{"TOKEN": "SECRET2", "A": "1"}, TimeoutSeconds: 5}
	info := s.auditInfo()
	got := strings.Join(append([]string{info.Argv0}, info.EnvNames...), " ")
	if strings.Contains(got, "SECRET") || info.Argc != 2 || got != "node A TOKEN" {
		t.Fatalf("auditInfo = %+v", info)
	}
}

func TestDropEnv(t *testing.T) {
	deny := []string{"PAPERCLIP_GIT_TOKEN", "GH_TOKEN", "GITHUB_TOKEN"}
	tests := []struct {
		name        string
		env         map[string]string
		deny        []string
		wantDropped []string
		wantLeft    []string
	}{
		{"nothing denied", map[string]string{"A": "1", "GH_TOKEN": "x"}, nil, nil, []string{"A=1", "GH_TOKEN=x"}},
		{"no env", nil, deny, nil, []string{}},
		{"drops and sorts", map[string]string{"GH_TOKEN": "x", "PAPERCLIP_GIT_TOKEN": "y", "A": "1"}, deny, []string{"GH_TOKEN", "PAPERCLIP_GIT_TOKEN"}, []string{"A=1"}},
		{"exact names only", map[string]string{"gh_token": "x", "GH_TOKEN_X": "y"}, deny, nil, []string{"GH_TOKEN_X=y", "gh_token=x"}},
		{"empty value is dropped too", map[string]string{"GITHUB_TOKEN": ""}, deny, []string{"GITHUB_TOKEN"}, []string{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := ExecSpec{Argv: []string{"env"}, Env: tt.env, TimeoutSeconds: 5}
			dropped := s.dropEnv(tt.deny)
			if strings.Join(dropped, ",") != strings.Join(tt.wantDropped, ",") {
				t.Fatalf("dropped %q, want %q", dropped, tt.wantDropped)
			}
			if got := s.envList(); strings.Join(got, ",") != strings.Join(tt.wantLeft, ",") {
				t.Fatalf("left %q, want %q", got, tt.wantLeft)
			}
			for _, d := range dropped {
				if slices.Contains(s.auditInfo().EnvNames, d) {
					t.Fatalf("audit still names the dropped %s as passed on", d)
				}
			}
		})
	}
}
