package sandbox

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/JCombee/wsm/internal/blocks"
	"github.com/JCombee/wsm/internal/db"
	"github.com/JCombee/wsm/internal/podman"
)

// Errors of the per-sandbox services (ADR 0015).
var (
	// ErrServiceUnavailable: the service could not be made ready now,
	// for instance because the sandbox is stopped, the manager is busy or
	// the container did not answer before the deadline. A later call may
	// succeed.
	ErrServiceUnavailable = errors.New("service not available")
	// ErrServiceDenied: the sandbox is gone or being deleted.
	ErrServiceDenied = errors.New("sandbox is being deleted")
	// ErrUnknownTag: the registry has no image for the block's tag.
	ErrUnknownTag = errors.New("tag does not exist")
)

// Limits of a per-sandbox service container: 0.5 CPU, 256 MiB, 64 pids.
const (
	serviceCPUQuota = 50000
	serviceMemBytes = 256 << 20
	servicePids     = 64
)

// lockPoll is how often a bounded wait for the manager's lock tries again.
const lockPoll = 50 * time.Millisecond

// readyPoll is how often EnsureService probes a starting service.
const readyPoll = 200 * time.Millisecond

// ServiceContainer returns the name of a per-sandbox service container.
func ServiceContainer(sandboxID, blockID string) string { return "sbx-" + sandboxID + "-" + blockID }

// serviceRuntime is how a per-sandbox block runs: its command and the probe
// that tells it is ready, with the exact output the probe must print.
type serviceRuntime struct {
	command []string
	probe   []string
	ready   string
}

// runtimeOf returns the runtime of a per-sandbox block.
func runtimeOf(blk blocks.Block) (serviceRuntime, error) {
	if blk.Kind != blocks.KindPerSandbox {
		return serviceRuntime{}, fmt.Errorf("block %s is not a per-sandbox service", blk.ID)
	}
	switch blk.ID {
	case "redis":
		port := strconv.Itoa(blk.ContainerPort)
		return serviceRuntime{
			// In memory only: no RDB snapshots and no AOF (ADR 0015).
			command: []string{"redis-server", "--bind", "127.0.0.1", "--port", port,
				"--save", "", "--appendonly", "no",
				"--maxmemory", "200mb", "--maxmemory-policy", "noeviction"},
			probe: []string{"redis-cli", "-h", "127.0.0.1", "-p", port, "ping"},
			ready: "PONG\n",
		}, nil
	}
	return serviceRuntime{}, fmt.Errorf("block %s has no runtime", blk.ID)
}

// ServiceSpec returns the container of a per-sandbox service in the
// sandbox's pod, with the hardening of CLAUDE.md invariant 9 and a
// read-only root filesystem. It has no volumes, no ports and no host
// paths; it shares the pod's network namespace, so only the pod reaches it
// on 127.0.0.1.
func ServiceSpec(p db.Project, sb db.Sandbox, blk blocks.Block, tag string) (podman.ContainerSpec, error) {
	rt, err := runtimeOf(blk)
	if err != nil {
		return podman.ContainerSpec{}, err
	}
	if err := blk.ValidTag(tag); err != nil {
		return podman.ContainerSpec{}, err
	}
	l := labels(p, sb)
	l["wsm.service"] = blk.ID
	l["wsm.tag"] = tag
	return podman.ContainerSpec{
		Name:               ServiceContainer(sb.ID, blk.ID),
		Image:              blk.ImageRef(tag),
		Pod:                PodName(sb.ID),
		Labels:             l,
		User:               strconv.Itoa(ContainerUID) + ":" + strconv.Itoa(ContainerGID),
		Command:            rt.command,
		CapDrop:            []string{"ALL"},
		NoNewPrivileges:    true,
		ReadOnlyFilesystem: true,
		ImageVolumeMode:    "tmpfs",
		Resources: &podman.Resources{
			CPU:    &podman.CPULimit{Quota: serviceCPUQuota, Period: cpuPeriod},
			Memory: &podman.MemoryLimit{Limit: serviceMemBytes},
			Pids:   &podman.PidsLimit{Limit: servicePids},
		},
	}, nil
}

// lockWithin takes m.mu, waiting no longer than ctx allows: image builds
// can hold it for minutes.
func (m *Manager) lockWithin(ctx context.Context) error {
	for !m.mu.TryLock() {
		select {
		case <-ctx.Done():
			return fmt.Errorf("%w: the manager is busy", ErrServiceUnavailable)
		case <-time.After(lockPoll):
		}
	}
	return nil
}

// EnsureService makes a per-sandbox service run in a sandbox's pod at tag
// and waits until it answers its probe. Under the manager's lock it checks
// that the sandbox is live and running, then calls record, which stores
// the choice (and takes the broker's lock: the order is always m.mu, then
// the broker's). An error from record is returned as it is, and the
// container is left alone. A container at another tag is replaced. The
// whole call is bounded by ctx.
func (m *Manager) EnsureService(ctx context.Context, sandboxID string, blk blocks.Block, tag string, record func(context.Context) error) error {
	rt, err := runtimeOf(blk)
	if err != nil {
		return err
	}
	if err := m.lockWithin(ctx); err != nil {
		return err
	}
	defer m.mu.Unlock()
	sb, err := m.Store.SandboxByID(ctx, sandboxID)
	if errors.Is(err, db.ErrNotFound) {
		return ErrServiceDenied
	}
	if err != nil {
		return fmt.Errorf("%w: %w", ErrServiceUnavailable, err)
	}
	if sb.MCPTokenHash == "" {
		return ErrServiceDenied
	}
	if sb.Status != db.StatusRunning {
		return fmt.Errorf("%w: the sandbox is %s", ErrServiceUnavailable, sb.Status)
	}
	p, err := m.Store.ProjectByID(ctx, sb.ProjectID)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrServiceUnavailable, err)
	}
	spec, err := ServiceSpec(p, sb, blk, tag)
	if err != nil {
		return err
	}
	if err := record(ctx); err != nil {
		return err
	}
	pc := m.Podman(p.LinuxUID)
	defer pc.Close()
	if err := m.runService(ctx, pc, spec, tag); err != nil {
		return fmt.Errorf("%w: %s: %w", ErrServiceUnavailable, spec.Name, err)
	}
	if err := waitReady(ctx, pc, spec.Name, rt); err != nil {
		return fmt.Errorf("%w: %s: %w", ErrServiceUnavailable, spec.Name, err)
	}
	return nil
}

// runService creates (replacing one at another tag) and starts a service
// container.
func (m *Manager) runService(ctx context.Context, pc *podman.Client, spec podman.ContainerSpec, tag string) error {
	exists := true
	st, err := pc.ContainerInspect(ctx, spec.Name)
	if podman.IsNotFound(err) {
		exists = false
	} else if err != nil {
		return err
	}
	if exists && st.Labels["wsm.tag"] != tag {
		m.logf("replacing %s: tag %q becomes %s", spec.Name, st.Labels["wsm.tag"], tag)
		if err := pc.ContainerRemove(ctx, spec.Name); err != nil {
			return err
		}
		exists = false
	}
	if !exists {
		if err := pc.ImagePull(ctx, spec.Image, podman.PullMissing); err != nil {
			return err
		}
		if _, err := pc.ContainerCreate(ctx, spec); err != nil {
			return err
		}
	}
	return pc.ContainerStart(ctx, spec.Name)
}

// waitReady runs the probe until its output is exactly rt.ready or ctx
// ends. The output comes from the sandbox: the exec reads it bounded, and
// it is only compared, never parsed (CLAUDE.md invariant 8).
func waitReady(ctx context.Context, pc *podman.Client, container string, rt serviceRuntime) error {
	for {
		out, err := pc.Output(ctx, container, rt.probe...)
		if err == nil && out == rt.ready {
			return nil
		}
		select {
		case <-ctx.Done():
			return errors.New("no answer before the deadline")
		case <-time.After(readyPoll):
		}
	}
}

// PullServiceImage pulls the image of a per-sandbox block's tag as the
// project user, so a ceiling only lists tags that exist. It does not take
// the manager's lock. A tag the registry does not know gives ErrUnknownTag.
func (m *Manager) PullServiceImage(ctx context.Context, projectID string, blk blocks.Block, tag string) error {
	if _, err := runtimeOf(blk); err != nil {
		return err
	}
	if err := blk.ValidTag(tag); err != nil {
		return err
	}
	p, err := m.Store.ProjectByID(ctx, projectID)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, imageTimeout)
	defer cancel()
	pc := m.Podman(p.LinuxUID)
	defer pc.Close()
	ref := blk.ImageRef(tag)
	if err := pc.ImagePull(ctx, ref, podman.PullMissing); err != nil {
		if strings.Contains(err.Error(), "manifest unknown") {
			return fmt.Errorf("%w: %s", ErrUnknownTag, ref)
		}
		return err
	}
	return nil
}
