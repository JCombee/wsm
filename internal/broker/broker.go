// Package broker manages the shared service instances and the project
// ceilings for blocks (plan section 3.6, ADR 0013), and hands per-sandbox
// services such as Redis to the sandbox manager (ADR 0015).
//
// Each (block, tag) has one instance under the service user, published on
// its own port on wsm0. The broker hands out ports, keeps each instance's
// root password sealed in the database, and asks wsm-helper to pull the
// image and ensure the instance. A project's ceiling lists the tags it may
// use; it is stored only once every instance it names is ready.
package broker

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"strconv"
	"sync"
	"time"

	"github.com/JCombee/wsm/internal/blocks"
	"github.com/JCombee/wsm/internal/db"
	"github.com/JCombee/wsm/internal/helper"
	"github.com/JCombee/wsm/internal/sandbox"
	"github.com/JCombee/wsm/internal/secret"
)

// ErrInvalid marks invalid input from the API (db.ErrInvalid, as in
// internal/sandbox).
var ErrInvalid = db.ErrInvalid

// Helper is the part of wsm-helper the broker uses.
type Helper interface {
	PullServiceImage(ctx context.Context, service, tag string) error
	EnsureServiceInstance(ctx context.Context, service, tag string, port int, password string) error
	AllowService(ctx context.Context, uid, port int) error
	RevokeService(ctx context.Context, uid, port int) error
}

// Pods is the part of the sandbox manager that runs per-sandbox services
// (ADR 0015). EnsureService takes the manager's lock before it calls
// record, and record takes grantMu: the lock order is always the
// manager's, then grantMu, as in a sandbox delete.
type Pods interface {
	PullServiceImage(ctx context.Context, projectID string, blk blocks.Block, tag string) error
	EnsureService(ctx context.Context, sandboxID string, blk blocks.Block, tag string, record func(context.Context) error) error
	RemoveService(ctx context.Context, sandboxID, blockID string) error
}

// Service states.
const (
	StateReady = "ready"
	StateDown  = "down"
)

// Ceiling is what a project may use of one block.
type Ceiling struct {
	Block   string
	Profile string
	// Tags are the allowed tags; the first one is the default.
	Tags []string
}

// Service is a shared instance with its live state.
type Service struct {
	Block string
	Tag   string
	// Address is host:port on wsm0.
	Address string
	Port    int
	// State is StateReady or StateDown, checked when it is listed.
	State string
	// Version is the server version from the greeting when ready.
	Version string
}

// Broker manages shared service instances and project ceilings.
type Broker struct {
	Store   *db.Store
	Helper  Helper
	Secrets *secret.Box
	// Addr is the wsm0 address the instances publish on.
	Addr netip.Addr
	// Dial connects to the instance on port. Nil dials Addr:port over TCP.
	Dial func(ctx context.Context, port int) (net.Conn, error)
	// ReadyTimeout bounds the wait for an instance to be ready; zero means
	// five minutes.
	ReadyTimeout time.Duration
	// PollInterval is the pause between readiness checks; zero means one
	// second.
	PollInterval time.Duration
	// SQL opens a root session on the instance on port. Nil uses
	// go-sql-driver/mysql on Addr:port.
	SQL func(ctx context.Context, port int, password string) (SQLConn, error)
	// GrantWait bounds how long Enable waits for a root login; zero means
	// 60 seconds.
	GrantWait time.Duration
	// Pods runs per-sandbox services in sandbox pods (the sandbox
	// manager). Nil means per-sandbox blocks are not available.
	Pods Pods
	Logf func(format string, args ...any)

	// mu serializes SetBlock, RemoveBlock and EnsureAll. It is separate
	// from the sandbox manager's lock, because a pull can take minutes.
	mu sync.Mutex
	// grantMu serializes grants, revocations and the ceiling checks that
	// depend on them. It is separate from mu, so service_enable does not
	// wait for a pull; whoever needs both takes mu first.
	grantMu sync.Mutex
}

func (b *Broker) logf(format string, args ...any) {
	if b.Logf != nil {
		b.Logf(format, args...)
	}
}

// config is the JSON in project_blocks.config_json.
type config struct {
	Tags []string `json:"tags"`
}

// newPassword returns a root password: 32 random bytes as base64url
// without padding, 43 characters.
func newPassword() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("generate password: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

// instance is a stored instance with its opened password. The password
// lives only in memory.
type instance struct {
	db.ServiceInstance
	password string
}

func (b *Broker) open(inst db.ServiceInstance) (instance, error) {
	pw, err := b.Secrets.Open(inst.SecretEnc, []byte(inst.ID))
	if err != nil {
		return instance{}, fmt.Errorf("open password of %s %s: %w", inst.BlockID, inst.Tag, err)
	}
	return instance{ServiceInstance: inst, password: string(pw)}, nil
}

// SetBlock sets the ceiling of a project for a block. For each tag without
// an instance it pulls the image first, then stores the instance with a
// new port and password. It ensures every instance of the tags, also the
// existing ones, waits until each one is ready, and only then stores the
// ceiling. When an ensure or the wait fails, the instance rows stay, so a
// next attempt uses the same port and password; the ceiling is unchanged.
func (b *Broker) SetBlock(ctx context.Context, projectSlug, blockID, profile string, tags []string) (Ceiling, error) {
	profile, err := blocks.ValidateCeiling(blockID, profile, tags)
	if err != nil {
		return Ceiling{}, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	blk, ok := blocks.Lookup(blockID)
	if !ok {
		return Ceiling{}, fmt.Errorf("%w: unknown block %q", ErrInvalid, blockID)
	}
	p, err := b.Store.ProjectBySlug(ctx, projectSlug)
	if err != nil {
		return Ceiling{}, err
	}
	// Fail early, before any pull; checked again below under grantMu.
	if err := b.checkCeilingKeepsGrants(ctx, p, blockID, tags); err != nil {
		return Ceiling{}, err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if blk.Kind == blocks.KindPerSandbox {
		if err := b.pullPerSandbox(ctx, p, blk, tags); err != nil {
			return Ceiling{}, err
		}
		return b.storeCeiling(ctx, p, blockID, profile, tags)
	}
	if blk.Kind != blocks.KindShared {
		return Ceiling{}, fmt.Errorf("%w: block %s has no ceiling", ErrInvalid, blockID)
	}

	insts := make([]instance, 0, len(tags))
	for _, tag := range tags {
		inst, err := b.instanceFor(ctx, blk, tag)
		if err != nil {
			return Ceiling{}, err
		}
		insts = append(insts, inst)
	}
	for _, inst := range insts {
		if err := b.Helper.EnsureServiceInstance(ctx, inst.BlockID, inst.Tag, inst.Port, inst.password); err != nil {
			return Ceiling{}, fmt.Errorf("ensure %s %s: %w", inst.BlockID, inst.Tag, err)
		}
	}
	for _, inst := range insts {
		if err := b.waitReady(ctx, inst.ServiceInstance); err != nil {
			return Ceiling{}, err
		}
	}
	return b.storeCeiling(ctx, p, blockID, profile, tags)
}

// pullPerSandbox pulls the image of each tag of a per-sandbox block as the
// project user, so the ceiling only lists tags that exist. Call with mu.
func (b *Broker) pullPerSandbox(ctx context.Context, p db.Project, blk blocks.Block, tags []string) error {
	if b.Pods == nil {
		return fmt.Errorf("block %s: per-sandbox services are not available", blk.ID)
	}
	for _, tag := range tags {
		if err := b.Pods.PullServiceImage(ctx, p.ID, blk, tag); err != nil {
			if errors.Is(err, sandbox.ErrUnknownTag) {
				return fmt.Errorf("%w: tag %s of block %s does not exist", ErrInvalid, tag, blk.ID)
			}
			return fmt.Errorf("pull %s %s for project %s: %w", blk.ID, tag, p.Slug, err)
		}
	}
	return nil
}

// storeCeiling checks again, under grantMu, that no sandbox uses a tag
// outside tags and stores the ceiling. Call with mu.
func (b *Broker) storeCeiling(ctx context.Context, p db.Project, blockID, profile string, tags []string) (Ceiling, error) {
	cfg, err := json.Marshal(config{Tags: tags})
	if err != nil {
		return Ceiling{}, fmt.Errorf("set block: %w", err)
	}
	b.grantMu.Lock()
	defer b.grantMu.Unlock()
	if err := b.checkCeilingKeepsGrants(ctx, p, blockID, tags); err != nil {
		return Ceiling{}, err
	}
	if err := b.Store.SetProjectBlock(ctx, db.ProjectBlock{ProjectID: p.ID, BlockID: blockID,
		Profile: profile, ConfigJSON: string(cfg)}); err != nil {
		return Ceiling{}, err
	}
	b.logf("project %s: block %s set to profile %s, tags %v", p.Slug, blockID, profile, tags)
	return Ceiling{Block: blockID, Profile: profile, Tags: append([]string(nil), tags...)}, nil
}

// instanceFor returns the instance of blk for tag, creating it when it does
// not exist: pull first, so a tag that does not exist leaves nothing.
func (b *Broker) instanceFor(ctx context.Context, blk blocks.Block, tag string) (instance, error) {
	stored, err := b.Store.ServiceInstance(ctx, blk.ID, tag)
	if err == nil {
		return b.open(stored)
	}
	if !errors.Is(err, db.ErrNotFound) {
		return instance{}, err
	}
	if err := b.Helper.PullServiceImage(ctx, blk.ID, tag); err != nil {
		if errors.Is(err, helper.ErrNotFound) {
			return instance{}, fmt.Errorf("%w: tag %s of block %s does not exist", ErrInvalid, tag, blk.ID)
		}
		return instance{}, fmt.Errorf("pull %s %s: %w", blk.ID, tag, err)
	}
	pw, err := newPassword()
	if err != nil {
		return instance{}, err
	}
	id, err := db.NewID()
	if err != nil {
		return instance{}, err
	}
	sealed, err := b.Secrets.Seal([]byte(pw), []byte(id))
	if err != nil {
		return instance{}, err
	}
	row := db.ServiceInstance{ID: id, BlockID: blk.ID, Tag: tag, SecretEnc: sealed}
	if err := b.Store.CreateServiceInstance(ctx, &row); err != nil {
		return instance{}, err
	}
	b.logf("service %s %s: new instance on port %d", blk.ID, tag, row.Port)
	return instance{ServiceInstance: row, password: pw}, nil
}

func (b *Broker) readyTimeout() time.Duration {
	if b.ReadyTimeout != 0 {
		return b.ReadyTimeout
	}
	return 5 * time.Minute
}

func (b *Broker) pollInterval() time.Duration {
	if b.PollInterval != 0 {
		return b.PollInterval
	}
	return time.Second
}

// waitReady waits until the instance answers with a greeting for its tag.
func (b *Broker) waitReady(ctx context.Context, inst db.ServiceInstance) error {
	ctx, cancel := context.WithTimeout(ctx, b.readyTimeout())
	defer cancel()
	var last error
	for {
		_, err := b.probe(ctx, inst)
		if err == nil {
			return nil
		}
		last = err
		select {
		case <-ctx.Done():
			return fmt.Errorf("service %s %s on port %d is not ready: %w", inst.BlockID, inst.Tag, inst.Port, last)
		case <-time.After(b.pollInterval()):
		}
	}
}

// probe connects to an instance and returns the server version from its
// greeting, if that version belongs to the instance's tag.
func (b *Broker) probe(ctx context.Context, inst db.ServiceInstance) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, greetingTimeout)
	defer cancel()
	conn, err := b.dial(ctx, inst.Port)
	if err != nil {
		return "", err
	}
	defer conn.Close()
	v, err := readGreeting(conn)
	if err != nil {
		return "", err
	}
	if !versionMatches(v, inst.Tag) {
		return "", fmt.Errorf("server version %q does not match tag %s", v, inst.Tag)
	}
	return v, nil
}

func (b *Broker) dial(ctx context.Context, port int) (net.Conn, error) {
	if b.Dial != nil {
		return b.Dial(ctx, port)
	}
	var d net.Dialer
	return d.DialContext(ctx, "tcp", b.address(port))
}

func (b *Broker) address(port int) string {
	return net.JoinHostPort(b.Addr.String(), strconv.Itoa(port))
}

// RemoveBlock removes the ceiling of a project for a block. The instances
// stay: other projects may use them. While sandboxes of the project have
// a grant on the block, or run it as a per-sandbox service, it fails with
// ErrConflict.
func (b *Broker) RemoveBlock(ctx context.Context, projectSlug, blockID string) error {
	p, err := b.Store.ProjectBySlug(ctx, projectSlug)
	if err != nil {
		return err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.grantMu.Lock()
	defer b.grantMu.Unlock()
	if err := b.checkCeilingKeepsGrants(ctx, p, blockID, nil); err != nil {
		return err
	}
	if err := b.Store.DeleteProjectBlock(ctx, p.ID, blockID); err != nil {
		return err
	}
	b.logf("project %s: block %s removed", p.Slug, blockID)
	return nil
}

// Blocks returns the ceilings of a project.
func (b *Broker) Blocks(ctx context.Context, projectSlug string) ([]Ceiling, error) {
	p, err := b.Store.ProjectBySlug(ctx, projectSlug)
	if err != nil {
		return nil, err
	}
	rows, err := b.Store.ProjectBlocks(ctx, p.ID)
	if err != nil {
		return nil, err
	}
	out := make([]Ceiling, 0, len(rows))
	for _, r := range rows {
		var cfg config
		if err := json.Unmarshal([]byte(r.ConfigJSON), &cfg); err != nil {
			return nil, fmt.Errorf("block %s of project %s: config: %w", r.BlockID, p.Slug, err)
		}
		out = append(out, Ceiling{Block: r.BlockID, Profile: r.Profile, Tags: cfg.Tags})
	}
	return out, nil
}

// Services returns every instance with its state, checked now.
func (b *Broker) Services(ctx context.Context) ([]Service, error) {
	insts, err := b.Store.ServiceInstances(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]Service, len(insts))
	var wg sync.WaitGroup
	for i, inst := range insts {
		out[i] = Service{Block: inst.BlockID, Tag: inst.Tag, Address: b.address(inst.Port),
			Port: inst.Port, State: StateDown}
		wg.Add(1)
		go func() {
			defer wg.Done()
			if v, err := b.probe(ctx, inst); err == nil {
				out[i].State, out[i].Version = StateReady, v
			}
		}()
	}
	wg.Wait()
	return out, nil
}

// EnsureAll ensures every stored instance, so units and secrets match the
// template again after an update or a lost unit. Errors are logged, not
// returned, and it does not wait for readiness.
func (b *Broker) EnsureAll(ctx context.Context) {
	b.mu.Lock()
	defer b.mu.Unlock()
	insts, err := b.Store.ServiceInstances(ctx)
	if err != nil {
		b.logf("ensure services: %v", err)
		return
	}
	for _, stored := range insts {
		inst, err := b.open(stored)
		if err != nil {
			b.logf("ensure services: %v", err)
			continue
		}
		if err := b.Helper.EnsureServiceInstance(ctx, inst.BlockID, inst.Tag, inst.Port, inst.password); err != nil {
			b.logf("ensure service %s %s: %v", inst.BlockID, inst.Tag, err)
			continue
		}
		b.logf("service %s %s ensured on port %d", inst.BlockID, inst.Tag, inst.Port)
	}
}
