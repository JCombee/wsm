package main

import (
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
