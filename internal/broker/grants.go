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
	if _, err := blocks.LookupShared(blockID); err != nil {
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

// Enable gives a sandbox an account on the instance of block for tag,
// within its project's ceiling, and returns the credentials. The account
// exists and the nftables element is in place before anything is
// returned (CLAUDE.md invariant 7). Calling it again returns the same
// credentials and repairs the account.
func (b *Broker) Enable(ctx context.Context, sandboxID, blockID, tag string) (Credentials, error) {
	p, sb, err := b.liveSandbox(ctx, sandboxID)
	if err != nil {
		return Credentials{}, err
	}
	tag, err = b.allowedTag(ctx, p.ID, blockID, tag)
	if err != nil {
		return Credentials{}, err
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
		out = append(out, o)
	}
	return out, nil
}

// Enabled returns the grants of a sandbox, without passwords.
func (b *Broker) Enabled(ctx context.Context, sandboxID string) ([]Enabled, error) {
	acct, err := accountName(sandboxID)
	if err != nil {
		return nil, err
	}
	grants, err := b.Store.GrantsBySandbox(ctx, sandboxID)
	if err != nil {
		return nil, err
	}
	out := make([]Enabled, 0, len(grants))
	for _, g := range grants {
		out = append(out, Enabled{Block: g.BlockID, Tag: g.Tag, Host: b.Addr.String(), Port: g.Port,
			User: acct, Database: appDatabase(sandboxID)})
	}
	return out, nil
}

// checkCeilingKeepsGrants fails with ErrConflict when sandboxes of the
// project have a grant on block outside tags (all of block when tags is
// empty).
func (b *Broker) checkCeilingKeepsGrants(ctx context.Context, p db.Project, blockID string, tags []string) error {
	slugs, err := b.Store.GrantsOutsideCeiling(ctx, p.ID, blockID, tags)
	if err != nil {
		return err
	}
	if len(slugs) > 0 {
		return fmt.Errorf("%w: block %s of project %s is in use by sandbox(es) %s; delete them first",
			ErrConflict, blockID, p.Slug, strings.Join(slugs, ", "))
	}
	return nil
}
