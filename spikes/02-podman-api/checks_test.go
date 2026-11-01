package main

import (
	"encoding/json"
	"strings"
	"testing"
)

const hardenedStatus = `Name:	cat
Umask:	0022
State:	R (running)
Uid:	1000	1000	1000	1000
Gid:	1000	1000	1000	1000
CapInh:	0000000000000000
CapPrm:	0000000000000000
CapEff:	0000000000000000
CapBnd:	0000000000000000
NoNewPrivs:	1
Seccomp:	2
Seccomp_filters:	1
`

func TestCheckProcHardening(t *testing.T) {
	tests := []struct {
		name    string
		replace [2]string
		want    string // substring of the only problem; "" means none
	}{
		{"hardened", [2]string{}, ""},
		{"new privs allowed", [2]string{"NoNewPrivs:\t1", "NoNewPrivs:\t0"}, "NoNewPrivs"},
		{"seccomp off", [2]string{"Seccomp:\t2", "Seccomp:\t0"}, "Seccomp"},
		{"effective caps", [2]string{"CapEff:\t0000000000000000", "CapEff:\t00000000a80425fb"}, "CapEff"},
		{"permitted caps", [2]string{"CapPrm:\t0000000000000000", "CapPrm:\t0000000000000001"}, "CapPrm"},
		{"root", [2]string{"Uid:\t1000\t1000\t1000\t1000", "Uid:\t0\t0\t0\t0"}, "Uid"},
		{"saved uid differs", [2]string{"Uid:\t1000\t1000\t1000\t1000", "Uid:\t1000\t1000\t0\t1000"}, "Uid"},
		{"missing seccomp", [2]string{"Seccomp:\t2\n", ""}, "Seccomp"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := hardenedStatus
			if tt.replace[0] != "" {
				s = strings.Replace(s, tt.replace[0], tt.replace[1], 1)
			}
			status, err := parseProcStatus(s)
			if err != nil {
				t.Fatal(err)
			}
			problems := checkProcHardening(status, 1000)
			if tt.want == "" {
				if len(problems) != 0 {
					t.Fatalf("problems = %q, want none", problems)
				}
				return
			}
			if len(problems) != 1 || !strings.Contains(problems[0], tt.want) {
				t.Fatalf("problems = %q, want one about %s", problems, tt.want)
			}
		})
	}
}

func TestParseProcStatusRejectsGarbage(t *testing.T) {
	for _, s := range []string{"no colon here", ":value without key"} {
		if _, err := parseProcStatus(s); err == nil {
			t.Errorf("parseProcStatus(%q) succeeded", s)
		}
	}
}

func TestParseIDMap(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		wantErr bool
		inside  uint64
		outside uint64
		maps    bool
	}{
		{"keep-id maps 1000 to the user", "         0          1       1000\n      1000          0          1\n      1001       1001      64536\n", false, 1000, 0, true},
		{"keep-id maps 0 to a subuid", "0 1 1000\n1000 0 1\n1001 1001 64536\n", false, 0, 1, true},
		{"default userns maps 1000 elsewhere", "0 0 1\n1 1 65536\n", false, 1000, 0, false},
		{"unmapped id", "0 0 1\n", false, 1000, 0, false},
		{"too few fields", "0 0\n", true, 0, 0, false},
		{"not a number", "0 x 1\n", true, 0, 0, false},
		{"empty range", "0 0 0\n", true, 0, 0, false},
		{"empty", "\n", true, 0, 0, false},
		{"too large", "0 0 4294967296\n", true, 0, 0, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ranges, err := parseIDMap(tt.in)
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
			if err != nil {
				return
			}
			if got := mapsTo(ranges, tt.inside, tt.outside); got != tt.maps {
				t.Fatalf("mapsTo(%d, %d) = %v, want %v", tt.inside, tt.outside, got, tt.maps)
			}
		})
	}
}

func TestCheckCgroupLimits(t *testing.T) {
	want := Limits{CPUQuota: 100000, CPUPeriod: 100000, Memory: 512 << 20, Pids: 256}
	tests := []struct {
		name             string
		cpu, mem, pids   string
		wantProblemCount int
	}{
		{"all set", "100000 100000\n", "536870912\n", "256\n", 0},
		{"no cpu limit", "max 100000\n", "536870912\n", "256\n", 1},
		{"no memory limit", "100000 100000\n", "max\n", "256\n", 1},
		{"no pids limit", "100000 100000\n", "536870912\n", "max\n", 1},
		{"nothing delegated", "", "", "", 3},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := checkCgroupLimits(tt.cpu, tt.mem, tt.pids, want)
			if len(got) != tt.wantProblemCount {
				t.Fatalf("problems = %q, want %d", got, tt.wantProblemCount)
			}
		})
	}
}

func TestCheckInspectHardening(t *testing.T) {
	var clean ContainerInspect
	if p := checkInspectHardening(clean); len(p) != 0 {
		t.Fatalf("clean container: %q", p)
	}
	var bad ContainerInspect
	bad.Mounts = make([]json.RawMessage, 1)
	bad.HostConfig.Privileged = true
	bad.HostConfig.NetworkMode = "host"
	if p := checkInspectHardening(bad); len(p) != 3 {
		t.Fatalf("bad container: %q, want 3 problems", p)
	}
}
