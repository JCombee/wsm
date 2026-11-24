package main

import (
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestStopScriptEndsAGroupThatIgnoresTerm runs the real wrapper and stop
// scripts under every shell a sandbox image may have as sh.
func TestStopScriptEndsAGroupThatIgnoresTerm(t *testing.T) {
	if _, err := exec.LookPath("timeout"); err != nil {
		t.Skip("no timeout(1)")
	}
	const ignoreTerm = `trap "" TERM; while :; do sleep 1; done`
	tests := []struct {
		shell  string
		prefix []string
		body   string
		want   string
	}{
		{"dash", nil, ignoreTerm, "kill"},
		{"bash", nil, ignoreTerm, "kill"},
		{"busybox", []string{"sh"}, ignoreTerm, "kill"},
		{"dash", nil, "sleep 300", "term"},
	}
	for _, tt := range tests {
		t.Run(tt.shell+"/"+tt.want, func(t *testing.T) {
			path, err := exec.LookPath(tt.shell)
			if err != nil {
				t.Skipf("no %s", tt.shell)
			}
			pidFile := filepath.Join(t.TempDir(), "pid")
			args := append(append([]string{}, tt.prefix...), "-c", wrapperScript, "s06-exec", pidFile, "60", "sh", "-c", tt.body)
			w := exec.Command(path, args...)
			if err := w.Start(); err != nil {
				t.Fatal(err)
			}
			done := make(chan error, 1)
			go func() { done <- w.Wait() }()
			time.Sleep(300 * time.Millisecond)

			args = append(append([]string{}, tt.prefix...), "-c", stopScript, "s06-stop", pidFile, "1")
			t0 := time.Now()
			out, err := exec.Command(path, args...).Output()
			if got := strings.TrimSpace(string(out)); err != nil || got != tt.want {
				_ = w.Process.Kill()
				t.Fatalf("stop script = %q, %v; want %q", got, err, tt.want)
			}
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				_ = w.Process.Kill()
				t.Fatal("the exec still runs 5 s after the stop script")
			}
			if took := time.Since(t0); took > 4*time.Second {
				t.Fatalf("stop took %v", took)
			}
		})
	}
}
