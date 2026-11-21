package broker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/JCombee/wsm/internal/blocks"
	"github.com/JCombee/wsm/internal/db"
	"github.com/JCombee/wsm/internal/helper"
	"github.com/JCombee/wsm/internal/sandbox"
)

// ErrDenied is returned when a sandbox asks for something outside its
// project's ceiling. The message is meant for the agent.
var ErrDenied = errors.New("denied")

// ErrUnavailable is returned when the instance cannot be reached in time.
var ErrUnavailable = errors.New("service not available")

// ErrConflict is returned when a ceiling would shrink below what
// sandboxes use.
var ErrConflict = errors.New("conflict")

// Credentials are what a sandbox gets for a grant. Only the
// service_enable tool returns them.
type Credentials struct {
	Block    string
	Tag      string
	Host     string
	Port     int
	User     string
	Password string
	Database string
}

// Offer is one block a sandbox may enable, with what it enabled already.
type Offer struct {
	Block       string
	Profile     string
	Tags        []string
	DefaultTag  string
	EnabledTags []string
}

// Enabled is a grant of a sandbox, without its password.
type Enabled struct {
	Block    string
	Tag      string
	Host     string
	Port     int
	User     string
	Database string
}

func (b *Broker) grantWait() time.Duration {
	if b.GrantWait != 0 {
		return b.GrantWait
	}
	return 60 * time.Second
}

// ceiling returns the ceiling of a project for a block, or false when the
// project has none for it.
func (b *Broker) ceiling(ctx context.Context, projectID, blockID string) (Ceiling, bool, error) {
	rows, err := b.Store.ProjectBlocks(ctx, projectID)
	if err != nil {
		return Ceiling{}, false, err
	}
	for _, r := range rows {
		if r.BlockID != blockID {
			continue
		}
		var cfg config
		if err := json.Unmarshal([]byte(r.ConfigJSON), &cfg); err != nil {
			return Ceiling{}, false, fmt.Errorf("block %s of project %s: config: %w", r.BlockID, projectID, err)
		}
		return Ceiling{Block: r.BlockID, Profile: r.Profile, Tags: cfg.Tags}, true, nil
	}
	return Ceiling{}, false, nil
}

// liveSandbox loads a sandbox and its project; a sandbox without a token
// hash is being deleted.
func (b *Broker) liveSandbox(ctx context.Context, sandboxID string) (db.Project, db.Sandbox, error) {
	sb, err := b.Store.SandboxByID(ctx, sandboxID)
	if err != nil {
		return db.Project{}, db.Sandbox{}, err
	}
	if sb.MCPTokenHash == "" {
		return db.Project{}, db.Sandbox{}, fmt.Errorf("%w: this sandbox is being deleted", ErrDenied)
	}
	p, err := b.Store.ProjectByID(ctx, sb.ProjectID)
	if err != nil {
		return db.Project{}, db.Sandbox{}, err
	}
	return p, sb, nil
}

// allowedTag checks block and tag against the ceiling of a project and
// returns the tag to use: an empty tag means the default.
func (b *Broker) allowedTag(ctx context.Context, projectID, blockID, tag string) (string, error) {
	if _, ok := blocks.Lookup(blockID); !ok {
		return "", fmt.Errorf("%w: unknown block %q", ErrDenied, blockID)
	}
	c, ok, err := b.ceiling(ctx, projectID, blockID)
	if err != nil {
		return "", err
	}
	if !ok || len(c.Tags) == 0 {
		return "", fmt.Errorf("%w: block %s is not allowed for this project", ErrDenied, blockID)
	}
	if tag == "" {
		return c.Tags[0], nil
	}
	if !slices.Contains(c.Tags, tag) {
		return "", fmt.Errorf("%w: tag %s of block %s is not allowed for this project (allowed: %s)",
			ErrDenied, tag, blockID, strings.Join(c.Tags, ", "))
	}
	return tag, nil
}

// waitRoot connects as root to an instance, retrying until the grant wait
// is over.
func (b *Broker) waitRoot(ctx context.Context, inst instance) (SQLConn, error) {
	ctx, cancel := context.WithTimeout(ctx, b.grantWait())
	defer cancel()
	for {
		conn, err := b.connectRoot(ctx, inst.Port, inst.password)
		if err == nil {
			return conn, nil
		}
		select {
		case <-ctx.Done():
			b.logf("service %s %s on port %d: no root login: %v", inst.BlockID, inst.Tag, inst.Port, err)
			return nil, fmt.Errorf("%w: %s %s", ErrUnavailable, inst.BlockID, inst.Tag)
		case <-time.After(b.pollInterval()):
		}
	}
}

// Enable gives a sandbox block at tag, within its project's ceiling, and
// returns how to reach it.
//
// For a shared block it creates an account on the instance of the tag. The
// account exists and the nftables element is in place before anything is
// returned (CLAUDE.md invariant 7). Calling it again returns the same
// credentials and repairs the account.
//
// For a per-sandbox block it runs the service in the sandbox's pod and
// returns once it answers; see enablePerSandbox.
func (b *Broker) Enable(ctx context.Context, sandboxID, blockID, tag string) (Credentials, error) {
	p, sb, err := b.liveSandbox(ctx, sandboxID)
	if err != nil {
		return Credentials{}, err
	}
	tag, err = b.allowedTag(ctx, p.ID, blockID, tag)
	if err != nil {
		return Credentials{}, err
	}
	if blk, _ := blocks.Lookup(blockID); blk.Kind == blocks.KindPerSandbox {
		return b.enablePerSandbox(ctx, p, sb, blk, tag)
	}
	stored, err := b.Store.ServiceInstance(ctx, blockID, tag)
	if errors.Is(err, db.ErrNotFound) {
		return Credentials{}, fmt.Errorf("%w: %s %s has no instance", ErrUnavailable, blockID, tag)
	}
	if err != nil {
		return Credentials{}, err
	}
	inst, err := b.open(stored)
	if err != nil {
		return Credentials{}, err
	}
	conn, err := b.waitRoot(ctx, inst)
	if err != nil {
		return Credentials{}, err
	}
	defer conn.Close()

	b.grantMu.Lock()
	defer b.grantMu.Unlock()
	// Again under the lock: a delete clears the token before it revokes.
	if p, sb, err = b.liveSandbox(ctx, sandboxID); err != nil {
		return Credentials{}, err
	}
	if _, err := b.allowedTag(ctx, p.ID, blockID, tag); err != nil {
		return Credentials{}, err
	}
	password, err := b.grantPassword(ctx, sb.ID, inst.ID)
	if err != nil {
		return Credentials{}, err
	}
	if err := createAccount(ctx, conn, sb.ID, password); err != nil {
		return Credentials{}, err
	}
	if err := b.Helper.AllowService(ctx, p.LinuxUID, inst.Port); err != nil {
		return Credentials{}, fmt.Errorf("allow %s %s for project %s: %w", blockID, tag, p.Slug, err)
	}
	acct, _ := accountName(sb.ID)
	b.logf("sandbox %s/%s: granted %s %s", p.Slug, sb.Slug, blockID, tag)
	return Credentials{Block: blockID, Tag: tag, Host: b.Addr.String(), Port: inst.Port,
		User: acct, Password: password, Database: appDatabase(sb.ID)}, nil
}

// localHost is where a per-sandbox service listens: the pod's loopback,
// which only the sandbox's own containers share.
const localHost = "127.0.0.1"

// enablePerSandbox runs a per-sandbox service at tag in the sandbox's pod
// (ADR 0015). The sandbox manager takes its lock (bounded by the grant
// wait), checks the sandbox, and calls record, which checks the token and
// the ceiling again under grantMu and stores the tag. Only then does it
// touch the container: a different tag replaces it. The credentials come
// after the service answered. When record succeeded but the container did
// not come up, the row keeps the new tag and a next call repairs it.
func (b *Broker) enablePerSandbox(ctx context.Context, p db.Project, sb db.Sandbox, blk blocks.Block, tag string) (Credentials, error) {
	if b.Pods == nil {
		return Credentials{}, fmt.Errorf("%w: %s %s", ErrUnavailable, blk.ID, tag)
	}
	ctx, cancel := context.WithTimeout(ctx, b.grantWait())
	defer cancel()
	record := func(ctx context.Context) error {
		b.grantMu.Lock()
		defer b.grantMu.Unlock()
		// Again under the lock: a delete clears the token first, and
		// the ceiling may have shrunk since the check in Enable.
		lp, lsb, err := b.liveSandbox(ctx, sb.ID)
		if err != nil {
			return err
		}
		if _, err := b.allowedTag(ctx, lp.ID, blk.ID, tag); err != nil {
			return err
		}
		return b.Store.SetSandboxService(ctx, lsb.ID, blk.ID, tag)
	}
	err := b.Pods.EnsureService(ctx, sb.ID, blk, tag, record)
	switch {
	case errors.Is(err, sandbox.ErrServiceDenied):
		return Credentials{}, fmt.Errorf("%w: this sandbox is being deleted", ErrDenied)
	case errors.Is(err, sandbox.ErrServiceUnavailable):
		b.logf("sandbox %s/%s: %s %s not available: %v", p.Slug, sb.Slug, blk.ID, tag, err)
		return Credentials{}, fmt.Errorf("%w: %s %s", ErrUnavailable, blk.ID, tag)
	case err != nil:
		return Credentials{}, err
	}
	b.logf("sandbox %s/%s: granted %s %s", p.Slug, sb.Slug, blk.ID, tag)
	return Credentials{Block: blk.ID, Tag: tag, Host: localHost, Port: blk.ContainerPort}, nil
}

// grantPassword returns the password of the grant of a sandbox on an
// instance, creating the grant when there is none. Call with grantMu.
func (b *Broker) grantPassword(ctx context.Context, sandboxID, instanceID string) (string, error) {
	g, err := b.Store.Grant(ctx, sandboxID, instanceID)
	if err == nil {
		pw, err := b.Secrets.Open(g.SecretEnc, []byte(g.ID))
		if err != nil {
			return "", fmt.Errorf("open password of grant %s: %w", g.ID, err)
		}
		if !helper.ValidPassword(string(pw)) {
			return "", fmt.Errorf("grant %s: stored password has the wrong form", g.ID)
		}
		return string(pw), nil
	}
	if !errors.Is(err, db.ErrNotFound) {
		return "", err
	}
	pw, err := newPassword()
	if err != nil {
		return "", err
	}
	id, err := db.NewID()
	if err != nil {
		return "", err
	}
	sealed, err := b.Secrets.Seal([]byte(pw), []byte(id))
	if err != nil {
		return "", err
	}
	if err := b.Store.CreateGrant(ctx, &db.Grant{ID: id, SandboxID: sandboxID, InstanceID: instanceID,
		SecretEnc: sealed}); err != nil {
		return "", err
	}
	return pw, nil
}

// RevokeSandbox revokes every grant of a sandbox: the account and its
// databases on the instance, then the nftables element when no other
// sandbox of the project has a grant there, then the row. It stops at the
// first error; the remaining rows stay, so a next call continues.
func (b *Broker) RevokeSandbox(ctx context.Context, sandboxID string) error {
	b.grantMu.Lock()
	defer b.grantMu.Unlock()
	grants, err := b.Store.GrantsBySandbox(ctx, sandboxID)
	if err != nil {
		return err
	}
	if len(grants) == 0 {
		return nil
	}
	sb, err := b.Store.SandboxByID(ctx, sandboxID)
	if err != nil {
		return err
	}
	p, err := b.Store.ProjectByID(ctx, sb.ProjectID)
	if err != nil {
		return err
	}
	for _, g := range grants {
		if err := b.revokeGrant(ctx, p, g); err != nil {
			return fmt.Errorf("revoke %s %s of sandbox %s/%s: %w", g.BlockID, g.Tag, p.Slug, sb.Slug, err)
		}
		b.logf("sandbox %s/%s: revoked %s %s", p.Slug, sb.Slug, g.BlockID, g.Tag)
	}
	return nil
}

func (b *Broker) revokeGrant(ctx context.Context, p db.Project, g db.Grant) error {
	stored, err := b.Store.ServiceInstance(ctx, g.BlockID, g.Tag)
	if err != nil {
		return err
	}
	inst, err := b.open(stored)
	if err != nil {
		return err
	}
	conn, err := b.connectRoot(ctx, inst.Port, inst.password)
	if err != nil {
		return err
	}
	defer conn.Close()
	if err := revokeAccount(ctx, conn, g.SandboxID); err != nil {
		return err
	}
	n, err := b.Store.CountProjectGrants(ctx, p.ID, g.InstanceID)
	if err != nil {
		return err
	}
	if n <= 1 {
		if err := b.Helper.RevokeService(ctx, p.LinuxUID, inst.Port); err != nil {
			return err
		}
	}
	return b.Store.DeleteGrant(ctx, g.ID)
}

// RestoreGrants adds the nftables element for every (project UID, port)
// pair with a grant, after ApplyBaseRules emptied the set. Errors are
// logged and counted; the rest goes on.
func (b *Broker) RestoreGrants(ctx context.Context) (int, error) {
	targets, err := b.Store.GrantTargets(ctx)
	if err != nil {
		return 0, err
	}
	failed := 0
	for _, t := range targets {
		if err := b.Helper.AllowService(ctx, t.UID, t.Port); err != nil {
			b.logf("restore grant uid %d port %d: %v", t.UID, t.Port, err)
			failed++
		}
	}
	b.logf("restored %d of %d service grants", len(targets)-failed, len(targets))
	return failed, nil
}

// Available returns the blocks in the ceiling of a sandbox's project, with
// the tags the sandbox enabled. It has no live state and no passwords.
func (b *Broker) Available(ctx context.Context, sandboxID string) ([]Offer, error) {
	sb, err := b.Store.SandboxByID(ctx, sandboxID)
	if err != nil {
		return nil, err
	}
	rows, err := b.Store.ProjectBlocks(ctx, sb.ProjectID)
	if err != nil {
		return nil, err
	}
	grants, err := b.Store.GrantsBySandbox(ctx, sandboxID)
	if err != nil {
		return nil, err
	}
	local, err := b.Store.SandboxServices(ctx, sandboxID)
	if err != nil {
		return nil, err
	}
	out := make([]Offer, 0, len(rows))
	for _, r := range rows {
		var cfg config
		if err := json.Unmarshal([]byte(r.ConfigJSON), &cfg); err != nil {
			return nil, fmt.Errorf("block %s: config: %w", r.BlockID, err)
		}
		o := Offer{Block: r.BlockID, Profile: r.Profile, Tags: cfg.Tags, EnabledTags: []string{}}
		if len(cfg.Tags) > 0 {
			o.DefaultTag = cfg.Tags[0]
		}
		for _, g := range grants {
			if g.BlockID == r.BlockID {
				o.EnabledTags = append(o.EnabledTags, g.Tag)
			}
		}
		for _, v := range local {
			if v.BlockID == r.BlockID {
				o.EnabledTags = append(o.EnabledTags, v.Tag)
			}
		}
		out = append(out, o)
	}
	return out, nil
}

// Enabled returns the grants of a sandbox, without passwords, followed by
// its per-sandbox services.
func (b *Broker) Enabled(ctx context.Context, sandboxID string) ([]Enabled, error) {
	acct, err := accountName(sandboxID)
	if err != nil {
		return nil, err
	}
	grants, err := b.Store.GrantsBySandbox(ctx, sandboxID)
	if err != nil {
		return nil, err
	}
	local, err := b.Store.SandboxServices(ctx, sandboxID)
	if err != nil {
		return nil, err
	}
	out := make([]Enabled, 0, len(grants)+len(local))
	for _, g := range grants {
		out = append(out, Enabled{Block: g.BlockID, Tag: g.Tag, Host: b.Addr.String(), Port: g.Port,
			User: acct, Database: appDatabase(sandboxID)})
	}
	for _, v := range local {
		blk, ok := blocks.Lookup(v.BlockID)
		if !ok {
			continue
		}
		out = append(out, Enabled{Block: v.BlockID, Tag: v.Tag, Host: localHost, Port: blk.ContainerPort})
	}
	return out, nil
}

// checkCeilingKeepsGrants fails with ErrConflict when sandboxes of the
// project have a grant on block, or run it as a per-sandbox service,
// outside tags (all of block when tags is empty).
func (b *Broker) checkCeilingKeepsGrants(ctx context.Context, p db.Project, blockID string, tags []string) error {
	slugs, err := b.Store.GrantsOutsideCeiling(ctx, p.ID, blockID, tags)
	if err != nil {
		return err
	}
	local, err := b.Store.SandboxServicesOutsideCeiling(ctx, p.ID, blockID, tags)
	if err != nil {
		return err
	}
	slugs = append(slugs, local...)
	slices.Sort(slugs)
	slugs = slices.Compact(slugs)
	if len(slugs) > 0 {
		return fmt.Errorf("%w: block %s of project %s is in use by sandbox(es) %s; delete them first",
			ErrConflict, blockID, p.Slug, strings.Join(slugs, ", "))
	}
	return nil
}

// Revoke takes one service away from a sandbox, from the dashboard: a
// shared-instance grant is revoked like in RevokeSandbox; a per-sandbox
// service container is removed. It fails with db.ErrNotFound when the
// sandbox has neither for blockID.
func (b *Broker) Revoke(ctx context.Context, sandboxID, blockID string) error {
	local, err := b.Store.SandboxServices(ctx, sandboxID)
	if err != nil {
		return err
	}
	for _, v := range local {
		if v.BlockID == blockID {
			if b.Pods == nil {
				return fmt.Errorf("block %s: per-sandbox services are not available", blockID)
			}
			return b.Pods.RemoveService(ctx, sandboxID, blockID)
		}
	}
	b.grantMu.Lock()
	defer b.grantMu.Unlock()
	grants, err := b.Store.GrantsBySandbox(ctx, sandboxID)
	if err != nil {
		return err
	}
	for _, g := range grants {
		if g.BlockID != blockID {
			continue
		}
		sb, err := b.Store.SandboxByID(ctx, sandboxID)
		if err != nil {
			return err
		}
		p, err := b.Store.ProjectByID(ctx, sb.ProjectID)
		if err != nil {
			return err
		}
		if err := b.revokeGrant(ctx, p, g); err != nil {
			return fmt.Errorf("revoke %s %s of sandbox %s/%s: %w", g.BlockID, g.Tag, p.Slug, sb.Slug, err)
		}
		b.logf("sandbox %s/%s: revoked %s %s from the dashboard", p.Slug, sb.Slug, g.BlockID, g.Tag)
		return nil
	}
	return fmt.Errorf("service %s of sandbox %s: %w", blockID, sandboxID, db.ErrNotFound)
}
