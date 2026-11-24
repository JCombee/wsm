package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
)

// TenantConfig is one project's entry in the ceiling. It lives in a file
// only root can write, never where a control room or an agent can.
type TenantConfig struct {
	// Project is the wsm project slug the control room may act on.
	Project string `json:"project"`
	// ControlRoomUID is the host UID of the control room's user. A
	// connection counts only when SO_PEERCRED reports exactly this UID.
	ControlRoomUID uint32 `json:"control_room_uid"`
	// SocketDir holds tenant.sock. It is owned by the server user, has the
	// control room's primary group and mode 0750.
	SocketDir string `json:"socket_dir"`
	// MaxSandboxes caps the project's sandboxes.
	MaxSandboxes int `json:"max_sandboxes"`
	// MaxExecs caps concurrent exec sessions; zero means 32.
	MaxExecs int `json:"max_execs,omitempty"`
}

// Config is the server's configuration.
type Config struct {
	AuditPath       string         `json:"audit_path"`
	AdminSocket     string         `json:"admin_socket"`
	PodmanSocketDir string         `json:"podman_socket_dir"`
	Tenants         []TenantConfig `json:"tenants"`
}

const (
	maxConfigSize   = 64 << 10
	defaultMaxExecs = 32
)

var slugRE = regexp.MustCompile(`^[a-z][a-z0-9-]{0,31}$`)

// LoadConfig reads and validates the configuration file.
func LoadConfig(path string) (Config, error) {
	f, err := os.Open(path)
	if err != nil {
		return Config{}, fmt.Errorf("config: %w", err)
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, maxConfigSize+1))
	if err != nil {
		return Config{}, fmt.Errorf("config: %w", err)
	}
	if len(data) > maxConfigSize {
		return Config{}, fmt.Errorf("config: larger than %d bytes", maxConfigSize)
	}
	var cfg Config
	if err := decodeStrict(bytes.NewReader(data), maxConfigSize, &cfg); err != nil {
		return Config{}, fmt.Errorf("config: %w", err)
	}
	if err := cfg.Validate(); err != nil {
		return Config{}, fmt.Errorf("config: %w", err)
	}
	return cfg, nil
}

func isCleanAbs(p string) bool {
	return filepath.IsAbs(p) && filepath.Clean(p) == p
}

// Validate checks the configuration and fills in defaults.
func (c *Config) Validate() error {
	for name, p := range map[string]string{
		"audit_path": c.AuditPath, "admin_socket": c.AdminSocket, "podman_socket_dir": c.PodmanSocketDir,
	} {
		if !isCleanAbs(p) {
			return fmt.Errorf("%s must be a clean absolute path", name)
		}
	}
	if len(c.Tenants) == 0 || len(c.Tenants) > 64 {
		return errors.New("tenants: need 1 to 64 entries")
	}
	projects := map[string]bool{}
	uids := map[uint32]bool{}
	dirs := map[string]bool{}
	for i := range c.Tenants {
		t := &c.Tenants[i]
		if !slugRE.MatchString(t.Project) {
			return fmt.Errorf("tenant %d: invalid project slug", i)
		}
		if t.ControlRoomUID == 0 {
			return fmt.Errorf("tenant %s: control_room_uid must not be root", t.Project)
		}
		if !isCleanAbs(t.SocketDir) {
			return fmt.Errorf("tenant %s: socket_dir must be a clean absolute path", t.Project)
		}
		if t.MaxSandboxes < 1 || t.MaxSandboxes > 64 {
			return fmt.Errorf("tenant %s: max_sandboxes must be 1 to 64", t.Project)
		}
		if t.MaxExecs == 0 {
			t.MaxExecs = defaultMaxExecs
		}
		if t.MaxExecs < 1 || t.MaxExecs > 256 {
			return fmt.Errorf("tenant %s: max_execs must be 1 to 256", t.Project)
		}
		if projects[t.Project] || uids[t.ControlRoomUID] || dirs[t.SocketDir] {
			return fmt.Errorf("tenant %s: project, uid and socket_dir must be unique", t.Project)
		}
		projects[t.Project], uids[t.ControlRoomUID], dirs[t.SocketDir] = true, true, true
	}
	return nil
}

// decodeStrict parses exactly one JSON value of at most limit bytes into
// out, refusing unknown fields and trailing data.
func decodeStrict(r io.Reader, limit int64, out any) error {
	data, err := io.ReadAll(io.LimitReader(r, limit+1))
	if err != nil {
		return err
	}
	if int64(len(data)) > limit {
		return fmt.Errorf("body larger than %d bytes", limit)
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(out); err != nil {
		return err
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return errors.New("trailing data after the JSON value")
	}
	return nil
}
