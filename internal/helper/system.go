package helper

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/netip"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/JCombee/wsm/internal/netpolicy"
)

// Fixed host locations. None of them comes from a request.
const (
	// PodmanSocketRoot holds one directory per project user with its Podman
	// API socket (ADR 0002).
	PodmanSocketRoot = "/run/wsm-podman"
	// HomeRoot holds the project users' home directories.
	HomeRoot   = "/home"
	tmpfilesD  = "/etc/tmpfiles.d"
	lingerDir  = "/var/lib/systemd/linger"
	resolvConf = "/etc/resolv.conf"
	// WSM0 is the dummy interface for shared services and the MCP server.
	WSM0 = "wsm0"
)

// toolPath is the only search path for external tools.
var toolPath = []string{"/usr/sbin", "/usr/bin", "/sbin", "/bin"}

// subIDCount is the number of subordinate UIDs and GIDs per project user.
const subIDCount = 65536

// HomeDir returns the home directory of the project user for uid.
func HomeDir(uid int) string { return filepath.Join(HomeRoot, UserName(uid)) }

// PodmanSocket returns the Podman API socket of the project user for uid.
func PodmanSocket(uid int) string {
	return filepath.Join(PodmanSocketRoot, UserName(uid), "podman.sock")
}

// SubIDRange returns the first subordinate ID of uid. A range derived from
// the UID cannot overlap the ranges useradd hands out, which start at 100000
// and grow by 65536 per user.
func SubIDRange(uid int) (first, last int64) {
	first = int64(uid) * subIDCount
	return first, first + subIDCount - 1
}

// SystemOps performs the privileged operations on the host. Every external
// tool gets fixed arguments; the only variable parts are the validated UID
// and names and paths derived from it.
type SystemOps struct {
	// ManagerGroup is the group that may use the project Podman sockets.
	ManagerGroup string
	// WSM0Addr is the IPv4 address of wsm0.
	WSM0Addr netip.Addr
	// MCPPort is the port of the MCP server on wsm0.
	MCPPort uint16
	// Logf receives progress messages; it may be nil.
	Logf func(format string, args ...any)
}

func (o *SystemOps) logf(format string, args ...any) {
	if o.Logf != nil {
		o.Logf(format, args...)
	}
}

// lookTool finds an external tool in the fixed search path.
func lookTool(name string) (string, error) {
	for _, dir := range toolPath {
		p := filepath.Join(dir, name)
		if fi, err := os.Stat(p); err == nil && fi.Mode().IsRegular() && fi.Mode()&0o111 != 0 {
			return p, nil
		}
	}
	return "", fmt.Errorf("%s not found in %s", name, strings.Join(toolPath, ":"))
}

// maxToolOutput caps the output kept from an external tool.
const maxToolOutput = 16 << 10

// run executes a tool with fixed arguments and returns its combined output.
func run(ctx context.Context, stdin io.Reader, env []string, name string, args ...string) (string, error) {
	path, err := lookTool(name)
	if err != nil {
		return "", err
	}
	cmd := exec.CommandContext(ctx, path, args...)
	cmd.Dir = "/"
	cmd.Env = append([]string{"PATH=" + strings.Join(toolPath, ":"), "LC_ALL=C"}, env...)
	cmd.Stdin = stdin
	var out cappedBuffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	cmd.WaitDelay = 5 * time.Second
	if err := cmd.Run(); err != nil {
		return out.String(), fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, strings.TrimSpace(out.String()))
	}
	return out.String(), nil
}

// cappedBuffer keeps the first maxToolOutput bytes written to it.
type cappedBuffer struct{ bytes.Buffer }

func (b *cappedBuffer) Write(p []byte) (int, error) {
	if room := maxToolOutput - b.Len(); room > 0 {
		b.Buffer.Write(p[:min(len(p), room)])
	}
	return len(p), nil
}

// lookupUser returns the passwd entry for the project user of uid. It
// fails when the name or the UID belongs to anything else.
func lookupUser(uid int) (*user.User, error) {
	name := UserName(uid)
	byName, nameErr := user.Lookup(name)
	byID, idErr := user.LookupId(strconv.Itoa(uid))
	var unknownName user.UnknownUserError
	var unknownID user.UnknownUserIdError
	switch {
	case errors.As(nameErr, &unknownName) && errors.As(idErr, &unknownID):
		return nil, nil
	case nameErr != nil && !errors.As(nameErr, &unknownName):
		return nil, fmt.Errorf("look up %s: %w", name, nameErr)
	case idErr != nil && !errors.As(idErr, &unknownID):
		return nil, fmt.Errorf("look up uid %d: %w", uid, idErr)
	case byName == nil || byID == nil || byName.Uid != byID.Uid || byName.Username != byID.Username:
		return nil, fmt.Errorf("uid %d and user %s do not belong together", uid, name)
	case byName.HomeDir != HomeDir(uid):
		return nil, fmt.Errorf("user %s has home %q, want %q", name, byName.HomeDir, HomeDir(uid))
	}
	return byName, nil
}

func (o *SystemOps) managerGID() (int, error) {
	g, err := user.LookupGroup(o.ManagerGroup)
	if err != nil {
		return 0, fmt.Errorf("look up group %s: %w", o.ManagerGroup, err)
	}
	return strconv.Atoi(g.Gid)
}

// CreateProjectUser creates the project user for uid: a system-less user
// with its own group, no password, no login shell, subordinate IDs, linger,
// and a Podman API socket that group ManagerGroup can use (ADR 0002, 0004).
// Running it again for an existing user finishes a partial earlier run.
func (o *SystemOps) CreateProjectUser(ctx context.Context, uid int) error {
	if !ValidUID(uid) {
		return fmt.Errorf("uid %d is outside %d-%d", uid, MinUID, MaxUID)
	}
	name := UserName(uid)
	home := HomeDir(uid)
	gid, err := o.managerGID()
	if err != nil {
		return err
	}
	u, err := lookupUser(uid)
	if err != nil {
		return err
	}
	if u == nil {
		o.logf("creating user %s", name)
		// A private group with the same ID as the user. useradd sets no
		// password, so the account is locked.
		if _, err := run(ctx, nil, nil, "groupadd", "--gid", strconv.Itoa(uid), name); err != nil &&
			!groupExists(name, uid) {
			return err
		}
		if _, err := run(ctx, nil, nil, "useradd",
			"--uid", strconv.Itoa(uid), "--gid", strconv.Itoa(uid),
			"--home-dir", home, "--create-home",
			"--shell", "/usr/sbin/nologin", "--comment", "wsm project",
			name); err != nil {
			return err
		}
	}
	if err := os.Chmod(home, 0o700); err != nil {
		return fmt.Errorf("chmod %s: %w", home, err)
	}
	if err := o.ensureSubIDs(ctx, uid); err != nil {
		return err
	}
	// The drop-in is written before the user's manager starts for the first
	// time, so no process of the user can race the root writes below.
	if err := writeSocketDropIn(uid); err != nil {
		return err
	}
	if err := writeTmpfiles(uid, gid); err != nil {
		return err
	}
	if _, err := run(ctx, nil, nil, "systemd-tmpfiles", "--create", tmpfilesFile(uid)); err != nil {
		return err
	}
	if _, err := run(ctx, nil, nil, "loginctl", "enable-linger", name); err != nil {
		return err
	}
	if err := waitFor(ctx, fmt.Sprintf("/run/user/%d/bus", uid), 60*time.Second); err != nil {
		return fmt.Errorf("user manager of %s: %w", name, err)
	}
	if _, err := run(ctx, nil, nil, "systemctl", "--user", "--machine", name+"@",
		"daemon-reload"); err != nil {
		return err
	}
	if _, err := run(ctx, nil, nil, "systemctl", "--user", "--machine", name+"@",
		"enable", "--now", "podman.socket"); err != nil {
		return err
	}
	if err := waitFor(ctx, PodmanSocket(uid), 30*time.Second); err != nil {
		return fmt.Errorf("podman socket of %s: %w", name, err)
	}
	o.logf("user %s ready", name)
	return nil
}

func groupExists(name string, gid int) bool {
	g, err := user.LookupGroup(name)
	return err == nil && g.Gid == strconv.Itoa(gid)
}

func (o *SystemOps) ensureSubIDs(ctx context.Context, uid int) error {
	name := UserName(uid)
	first, last := SubIDRange(uid)
	r := fmt.Sprintf("%d-%d", first, last)
	for _, f := range []struct{ file, flag string }{
		{"/etc/subuid", "--add-subuids"},
		{"/etc/subgid", "--add-subgids"},
	} {
		has, err := hasSubIDEntry(f.file, name)
		if err != nil {
			return err
		}
		if !has {
			if _, err := run(ctx, nil, nil, "usermod", f.flag, r, name); err != nil {
				return err
			}
		}
	}
	return nil
}

// hasSubIDEntry reports whether file has a line for name.
func hasSubIDEntry(file, name string) (bool, error) {
	data, err := os.ReadFile(file)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("read %s: %w", file, err)
	}
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, name+":") {
			return true, nil
		}
	}
	return false, nil
}

// writeSocketDropIn moves the user's Podman socket to PodmanSocket(uid)
// with mode 0660 (ADR 0002). The files belong to the user, as the rest of
// its ~/.config does.
func writeSocketDropIn(uid int) error {
	home := HomeDir(uid)
	dirs := []string{
		filepath.Join(home, ".config"),
		filepath.Join(home, ".config", "systemd"),
		filepath.Join(home, ".config", "systemd", "user"),
		filepath.Join(home, ".config", "systemd", "user", "podman.socket.d"),
	}
	for _, d := range dirs {
		if err := os.Mkdir(d, 0o755); err != nil && !errors.Is(err, os.ErrExist) {
			return fmt.Errorf("mkdir %s: %w", d, err)
		}
		fi, err := os.Lstat(d)
		if err != nil {
			return fmt.Errorf("stat %s: %w", d, err)
		}
		if !fi.IsDir() {
			return fmt.Errorf("%s is not a directory", d)
		}
		if err := os.Lchown(d, uid, uid); err != nil {
			return fmt.Errorf("chown %s: %w", d, err)
		}
	}
	content := fmt.Sprintf("# Written by wsm-helper (ADR 0002).\n[Socket]\nListenStream=\nListenStream=%s\nSocketMode=0660\n",
		PodmanSocket(uid))
	return writeFileNoFollow(filepath.Join(dirs[len(dirs)-1], "wsm.conf"), []byte(content), 0o644, uid, uid)
}

func tmpfilesFile(uid int) string {
	return filepath.Join(tmpfilesD, fmt.Sprintf("wsm-podman-%d.conf", uid))
}

// writeTmpfiles makes systemd create the socket directory at every boot:
// owned by the project user, group gid, setgid, so the socket gets the group.
func writeTmpfiles(uid, gid int) error {
	content := fmt.Sprintf("# Written by wsm-helper (ADR 0002).\nd %s 2750 %d %d -\n",
		filepath.Dir(PodmanSocket(uid)), uid, gid)
	return writeFileNoFollow(tmpfilesFile(uid), []byte(content), 0o644, 0, 0)
}

// writeFileNoFollow replaces path with data, refusing to follow a symlink.
func writeFileNoFollow(path string, data []byte, mode os.FileMode, uid, gid int) error {
	if fi, err := os.Lstat(path); err == nil && !fi.Mode().IsRegular() {
		return fmt.Errorf("%s exists and is not a regular file", path)
	}
	tmp := path + ".tmp"
	os.Remove(tmp)
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscallNoFollow, mode)
	if err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	_, werr := f.Write(data)
	if werr == nil {
		werr = f.Chown(uid, gid)
	}
	if werr == nil {
		werr = f.Chmod(mode)
	}
	if cerr := f.Close(); werr == nil {
		werr = cerr
	}
	if werr == nil {
		werr = os.Rename(tmp, path)
	}
	if werr != nil {
		os.Remove(tmp)
		return fmt.Errorf("write %s: %w", path, werr)
	}
	return nil
}

func waitFor(ctx context.Context, path string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		if _, err := os.Stat(path); err == nil {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("%s did not appear within %s", path, timeout)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(250 * time.Millisecond):
		}
	}
}

// DeleteProjectUser removes the project user for uid with everything it
// owns: processes, Podman state, home, subordinate IDs, linger, socket
// directory and tmpfiles entry. A missing user is not an error, so a
// partial earlier run can be finished.
func (o *SystemOps) DeleteProjectUser(ctx context.Context, uid int) error {
	if !ValidUID(uid) {
		return fmt.Errorf("uid %d is outside %d-%d", uid, MinUID, MaxUID)
	}
	name := UserName(uid)
	u, err := lookupUser(uid)
	if err != nil {
		return err
	}
	if u != nil {
		o.logf("deleting user %s", name)
		// Best effort: remove the user's containers, images and volumes
		// while its Podman still runs.
		if _, err := os.Stat(fmt.Sprintf("/run/user/%d/bus", uid)); err == nil {
			if _, err := run(ctx, nil, []string{
				"HOME=" + HomeDir(uid),
				fmt.Sprintf("XDG_RUNTIME_DIR=/run/user/%d", uid),
				fmt.Sprintf("DBUS_SESSION_BUS_ADDRESS=unix:path=/run/user/%d/bus", uid),
			}, "runuser", "-u", name, "--", "podman", "system", "reset", "--force"); err != nil {
				o.logf("podman system reset for %s: %v", name, err)
			}
		}
		if _, err := run(ctx, nil, nil, "loginctl", "disable-linger", name); err != nil {
			o.logf("disable linger for %s: %v", name, err)
		}
		if _, err := run(ctx, nil, nil, "systemctl", "stop", fmt.Sprintf("user@%d.service", uid)); err != nil {
			o.logf("stop user manager of %s: %v", name, err)
		}
		if _, err := run(ctx, nil, nil, "loginctl", "terminate-user", name); err != nil {
			o.logf("terminate %s: %v", name, err)
		}
		if err := waitNoProcesses(ctx, uid, 30*time.Second); err != nil {
			return err
		}
		first, last := SubIDRange(uid)
		r := fmt.Sprintf("%d-%d", first, last)
		if _, err := run(ctx, nil, nil, "usermod", "--del-subuids", r, "--del-subgids", r, name); err != nil {
			o.logf("remove subordinate ids of %s: %v", name, err)
		}
		if _, err := run(ctx, nil, nil, "userdel", "--remove", name); err != nil {
			// userdel exits 12 when it could not remove the home; that is
			// handled below.
			if _, lerr := user.Lookup(name); lerr == nil {
				return err
			}
		}
	}
	if groupExists(name, uid) {
		if _, err := run(ctx, nil, nil, "groupdel", name); err != nil {
			return err
		}
	}
	// What userdel or logind may leave behind. All paths are derived from
	// the UID. RemoveAll does not follow symlinks below the root it is given.
	for _, p := range []string{
		HomeDir(uid),
		filepath.Dir(PodmanSocket(uid)),
		tmpfilesFile(uid),
		filepath.Join(lingerDir, name),
	} {
		if err := os.RemoveAll(p); err != nil {
			return fmt.Errorf("remove %s: %w", p, err)
		}
	}
	for _, f := range []string{"/etc/subuid", "/etc/subgid"} {
		if has, err := hasSubIDEntry(f, name); err != nil {
			return err
		} else if has {
			return fmt.Errorf("%s still has an entry for %s", f, name)
		}
	}
	o.logf("user %s removed", name)
	return nil
}

// waitNoProcesses waits until no process runs as uid.
func waitNoProcesses(ctx context.Context, uid int, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		n, err := countProcesses(uid)
		if err != nil {
			return err
		}
		if n == 0 {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("%d processes of uid %d still run after %s", n, uid, timeout)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(250 * time.Millisecond):
		}
	}
}

func countProcesses(uid int) (int, error) {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return 0, fmt.Errorf("read /proc: %w", err)
	}
	n := 0
	for _, e := range entries {
		if _, err := strconv.Atoi(e.Name()); err != nil {
			continue
		}
		fi, err := os.Stat(filepath.Join("/proc", e.Name()))
		if err != nil {
			continue
		}
		if st, ok := statUID(fi); ok && st == uid {
			n++
		}
	}
	return n, nil
}

// SetupWSM0 creates the dummy interface wsm0 with WSM0Addr/32, or brings an
// existing one to that state.
func (o *SystemOps) SetupWSM0(ctx context.Context) error {
	if !o.WSM0Addr.Is4() {
		return fmt.Errorf("wsm0 address %v is not IPv4", o.WSM0Addr)
	}
	if _, err := run(ctx, nil, nil, "ip", "link", "show", "dev", WSM0); err != nil {
		if _, err := run(ctx, nil, nil, "ip", "link", "add", WSM0, "type", "dummy"); err != nil {
			return err
		}
	}
	if _, err := run(ctx, nil, nil, "ip", "addr", "replace", o.WSM0Addr.String()+"/32", "dev", WSM0); err != nil {
		return err
	}
	if _, err := run(ctx, nil, nil, "ip", "link", "set", WSM0, "up"); err != nil {
		return err
	}
	return nil
}

// ApplyBaseRules loads the base ruleset of table inet wsm, with the host
// resolvers from /etc/resolv.conf.
func (o *SystemOps) ApplyBaseRules(ctx context.Context) error {
	f, err := os.Open(resolvConf)
	if err != nil {
		return fmt.Errorf("open %s: %w", resolvConf, err)
	}
	resolvers, err := netpolicy.ParseResolvConf(f)
	f.Close()
	if err != nil {
		return err
	}
	cfg := netpolicy.DefaultConfig(resolvers)
	cfg.WSM0Addr = o.WSM0Addr
	cfg.MCPPort = o.MCPPort
	script, err := netpolicy.Base(cfg)
	if err != nil {
		return err
	}
	if _, err := run(ctx, strings.NewReader(script), nil, "nft", "-f", "-"); err != nil {
		return err
	}
	return nil
}
