package main

import (
	"fmt"
	"strconv"
	"strings"
)

// parseProcStatus parses /proc/<pid>/status into key/value pairs.
func parseProcStatus(s string) (map[string]string, error) {
	m := make(map[string]string)
	for i, line := range strings.Split(s, "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		k, v, ok := strings.Cut(line, ":")
		if !ok || k == "" {
			return nil, fmt.Errorf("line %d: no key: %q", i+1, line)
		}
		m[k] = strings.TrimSpace(v)
	}
	return m, nil
}

// checkProcHardening returns what is wrong with a process's status for a
// workspace container: it must have no_new_privs, seccomp filtering, no
// capabilities and run as wantUID.
func checkProcHardening(status map[string]string, wantUID int) []string {
	var problems []string
	if v := status["NoNewPrivs"]; v != "1" {
		problems = append(problems, fmt.Sprintf("NoNewPrivs=%q, want 1", v))
	}
	if v := status["Seccomp"]; v != "2" {
		problems = append(problems, fmt.Sprintf("Seccomp=%q, want 2 (filter)", v))
	}
	for _, k := range []string{"CapEff", "CapPrm"} {
		v, ok := status[k]
		n, err := strconv.ParseUint(v, 16, 64)
		if !ok || err != nil || n != 0 {
			problems = append(problems, fmt.Sprintf("%s=%q, want 0", k, v))
		}
	}
	if p := checkIDs(status["Uid"], wantUID); p != "" {
		problems = append(problems, "Uid "+p)
	}
	if p := checkIDs(status["Gid"], wantUID); p != "" {
		problems = append(problems, "Gid "+p)
	}
	return problems
}

// checkIDs checks a status Uid or Gid line (real, effective, saved, fs).
func checkIDs(line string, want int) string {
	fields := strings.Fields(line)
	if len(fields) != 4 {
		return fmt.Sprintf("%q: want 4 fields", line)
	}
	for _, f := range fields {
		if f != strconv.Itoa(want) {
			return fmt.Sprintf("%q, want all %d", line, want)
		}
	}
	return ""
}

// idRange is one line of /proc/<pid>/uid_map or gid_map.
type idRange struct {
	inside, outside, size uint64
}

// parseIDMap parses a uid_map or gid_map.
func parseIDMap(s string) ([]idRange, error) {
	var ranges []idRange
	for i, line := range strings.Split(s, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		if len(fields) != 3 {
			return nil, fmt.Errorf("line %d: want 3 fields: %q", i+1, line)
		}
		var nums [3]uint64
		for j, f := range fields {
			n, err := strconv.ParseUint(f, 10, 32)
			if err != nil {
				return nil, fmt.Errorf("line %d: %w", i+1, err)
			}
			nums[j] = n
		}
		if nums[2] == 0 {
			return nil, fmt.Errorf("line %d: empty range", i+1)
		}
		ranges = append(ranges, idRange{nums[0], nums[1], nums[2]})
	}
	if len(ranges) == 0 {
		return nil, fmt.Errorf("empty id map")
	}
	return ranges, nil
}

// mapsTo reports whether ID inside maps to outside in the parent namespace.
func mapsTo(ranges []idRange, inside, outside uint64) bool {
	for _, r := range ranges {
		if inside >= r.inside && inside < r.inside+r.size {
			return r.outside+(inside-r.inside) == outside
		}
	}
	return false
}

// Limits are the resource limits the spike puts on the workspace container.
type Limits struct {
	CPUQuota  int64
	CPUPeriod uint64
	Memory    int64
	Pids      int64
}

// checkCgroupLimits compares the cgroup v2 files cpu.max, memory.max and
// pids.max, as seen inside the container, with want.
func checkCgroupLimits(cpuMax, memoryMax, pidsMax string, want Limits) []string {
	var problems []string
	wantCPU := fmt.Sprintf("%d %d", want.CPUQuota, want.CPUPeriod)
	if got := strings.Join(strings.Fields(cpuMax), " "); got != wantCPU {
		problems = append(problems, fmt.Sprintf("cpu.max=%q, want %q", got, wantCPU))
	}
	if got := strings.TrimSpace(memoryMax); got != strconv.FormatInt(want.Memory, 10) {
		problems = append(problems, fmt.Sprintf("memory.max=%q, want %d", got, want.Memory))
	}
	if got := strings.TrimSpace(pidsMax); got != strconv.FormatInt(want.Pids, 10) {
		problems = append(problems, fmt.Sprintf("pids.max=%q, want %d", got, want.Pids))
	}
	return problems
}

// checkInspectHardening checks the container's own configuration: no mounts,
// not privileged, no host network.
func checkInspectHardening(ci ContainerInspect) []string {
	var problems []string
	if n := len(ci.Mounts); n != 0 {
		problems = append(problems, fmt.Sprintf("%d mounts, want none", n))
	}
	if ci.HostConfig.Privileged {
		problems = append(problems, "privileged")
	}
	if ci.HostConfig.NetworkMode == "host" {
		problems = append(problems, "host network")
	}
	return problems
}
