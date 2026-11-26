package main

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sync"
	"time"
)

// ExecAudit is what the audit keeps of an exec: never argv[1:] and never an
// environment value, because either can hold a secret.
type ExecAudit struct {
	Argv0          string   `json:"argv0"`
	Argc           int      `json:"argc"`
	EnvNames       []string `json:"env_names"`
	EnvDropped     []string `json:"env_dropped,omitempty"`
	TimeoutSeconds int      `json:"timeout_seconds"`
	Stdin          bool     `json:"stdin"`
	ExitCode       *int     `json:"exit_code,omitempty"`
	TimedOut       bool     `json:"timed_out,omitempty"`
	Stopped        bool     `json:"stopped,omitempty"`
	DurationMS     int64    `json:"duration_ms,omitempty"`
	BytesIn        int64    `json:"bytes_in,omitempty"`
	BytesOut       int64    `json:"bytes_out,omitempty"`
	BytesErr       int64    `json:"bytes_err,omitempty"`
}

// Record is one audit line.
type Record struct {
	Time     string     `json:"time"`
	Project  string     `json:"project"`
	PeerUID  uint32     `json:"peer_uid"`
	PeerPID  int32      `json:"peer_pid"`
	ReqID    string     `json:"req_id,omitempty"`
	Method   string     `json:"method"`
	Route    string     `json:"route"`
	Sandbox  string     `json:"sandbox,omitempty"`
	Decision string     `json:"decision"`
	Status   int        `json:"status,omitempty"`
	Reason   string     `json:"reason,omitempty"`
	Exec     *ExecAudit `json:"exec,omitempty"`
	// Prev is the sha256 of the previous line, which chains the lines.
	Prev string `json:"prev"`
}

// Audit appends hash-chained JSON lines to a file. Each Write opens the
// file, appends one line and syncs it before it returns, so a caller that
// answers only after a successful Write never answers an unaudited call.
type Audit struct {
	mu   sync.Mutex
	path string
	prev string
}

// maxAuditLine bounds one line when the file is read back.
const maxAuditLine = 1 << 20

// OpenAudit continues the chain in path, which must exist.
func OpenAudit(path string) (*Audit, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("audit: %w", err)
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64<<10), maxAuditLine)
	var last []byte
	for sc.Scan() {
		last = append(last[:0], sc.Bytes()...)
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("audit: %w", err)
	}
	a := &Audit{path: path}
	if len(last) > 0 {
		a.prev = hashLine(last)
	}
	return a, nil
}

func hashLine(line []byte) string {
	sum := sha256.Sum256(line)
	return hex.EncodeToString(sum[:])
}

// Write appends r and syncs the file. On error nothing counts as written
// and the chain does not advance.
func (a *Audit) Write(r Record) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	r.Prev = a.prev
	if r.Time == "" {
		r.Time = time.Now().UTC().Format(time.RFC3339Nano)
	}
	line, err := json.Marshal(r)
	if err != nil {
		return fmt.Errorf("audit: %w", err)
	}
	// No O_CREATE: a missing file is an error, not a fresh chain.
	f, err := os.OpenFile(a.path, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		return fmt.Errorf("audit: %w", err)
	}
	_, werr := f.Write(append(line, '\n'))
	serr := f.Sync()
	cerr := f.Close()
	for _, e := range []error{werr, serr, cerr} {
		if e != nil {
			return fmt.Errorf("audit: %w", e)
		}
	}
	a.prev = hashLine(line)
	return nil
}

// VerifyAudit checks the chain and returns the number of lines. Changing
// any line breaks the prev of the line after it.
func VerifyAudit(r io.Reader) (int, error) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64<<10), maxAuditLine)
	prev := ""
	n := 0
	for sc.Scan() {
		n++
		line := sc.Bytes()
		var rec struct {
			Prev *string `json:"prev"`
		}
		dec := json.NewDecoder(bytes.NewReader(line))
		if err := dec.Decode(&rec); err != nil {
			return n, fmt.Errorf("line %d: %w", n, err)
		}
		if rec.Prev == nil || *rec.Prev != prev {
			return n, fmt.Errorf("line %d: prev does not match line %d", n, n-1)
		}
		prev = hashLine(line)
	}
	if err := sc.Err(); err != nil {
		return n, fmt.Errorf("line %d: %w", n+1, err)
	}
	return n, nil
}
