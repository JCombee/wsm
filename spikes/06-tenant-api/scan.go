package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// runScan watches /proc/*/cmdline for needles until a stop file appears or
// the time is up. The needles come from the environment (S06_NEEDLES, one
// "name:value" per line), so the scanner's own argv never holds them. It
// prints per name how many processes showed the value and their comm, never
// the value itself.
func runScan(args []string) error {
	fs := flag.NewFlagSet("scan", flag.ContinueOnError)
	stop := fs.String("stop", "", "stop when this file exists")
	limit := fs.Duration("for", 20*time.Minute, "longest scan")
	if err := fs.Parse(args); err != nil {
		return err
	}
	type needle struct {
		name  string
		value []byte
	}
	var needles []needle
	for line := range strings.SplitSeq(os.Getenv("S06_NEEDLES"), "\n") {
		name, value, ok := strings.Cut(line, ":")
		if ok && len(value) >= 8 {
			needles = append(needles, needle{name, []byte(value)})
		}
	}
	if len(needles) == 0 {
		return errors.New("scan: no needles in S06_NEEDLES")
	}
	self := os.Getpid()
	hits := map[string]map[int]string{}
	for _, n := range needles {
		hits[n.name] = map[int]string{}
	}
	scans, unreadable := 0, 0
	deadline := time.Now().Add(*limit)
	for time.Now().Before(deadline) {
		if *stop != "" {
			if _, err := os.Stat(*stop); err == nil {
				break
			}
		}
		paths, _ := filepath.Glob("/proc/[0-9]*/cmdline")
		for _, p := range paths {
			var pid int
			if _, err := fmt.Sscanf(p, "/proc/%d/cmdline", &pid); err != nil || pid == self {
				continue
			}
			data, err := os.ReadFile(p)
			if err != nil {
				unreadable++
				continue
			}
			for _, n := range needles {
				if bytes.Contains(data, n.value) {
					if _, seen := hits[n.name][pid]; !seen {
						comm, _ := os.ReadFile(filepath.Join(filepath.Dir(p), "comm"))
						hits[n.name][pid] = strings.TrimSpace(string(comm))
					}
				}
			}
		}
		scans++
		time.Sleep(200 * time.Millisecond)
	}
	out := map[string]any{"scans": scans, "unreadable": unreadable, "uid": os.Getuid()}
	for name, m := range hits {
		var comms []string
		for _, c := range m {
			comms = append(comms, c)
		}
		out[name] = map[string]any{"processes": len(m), "comms": comms}
	}
	b, _ := json.Marshal(out)
	fmt.Println(string(b))
	return nil
}
