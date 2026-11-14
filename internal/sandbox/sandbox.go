// Package sandbox manages the life cycle of projects and their sandboxes:
// the project user (through wsm-helper), and per sandbox a rootless pod
// with a hardened workspace container (plan section 3.4).
package sandbox

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"sync"
	"time"

	"github.com/JCombee/wsm/internal/db"
	"github.com/JCombee/wsm/internal/gitmgr"
	"github.com/JCombee/wsm/internal/podman"
	"github.com/JCombee/wsm/internal/secret"
)

// The user and group the workspace runs as inside the container. The pod's
// keep-id user namespace maps them to the project user on the host.
const (
	ContainerUID = 1000
	ContainerGID = 1000
)

// Mount points in the workspace container.
const (
	WorkspaceDir = "/workspace"
	HomeDir      = "/home/agent"
)

// cpuPeriod is the CFS period for the CPU limit, in microseconds.
const cpuPeriod = 100000

// stopTimeout is how long a stopping container gets before it is killed.
const stopTimeout = 10 * time.Second

// imageTimeout bounds one image pull or build. The project user's Podman
// runs it, so it must not hold the manager forever.
const imageTimeout = 30 * time.Minute

// Limits for project settings.
const (
	MinCPUMillis = 100
	MaxCPUMillis = 64000
	MinMemBytes  = 64 << 20
	MaxMemBytes  = 256 << 30
	MinPids      = 64
	MaxPids      = 65536
)

// Defaults for project settings.
const (
	DefaultCPUMillis = 2000
	DefaultMemBytes  = 2 << 30
	DefaultPids      = 1024
)

var (
	slugPattern  = regexp.MustCompile(`^[a-z]([a-z0-9-]{0,30}[a-z0-9])?$`)
	imagePattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._/:@-]{0,254}$`)
)

// ErrInvalid marks invalid input from the API. It is db.ErrInvalid, so
// the store's own checks map to the same error.
var ErrInvalid = db.ErrInvalid

// ErrState marks an operation that the sandbox's state does not allow.
var ErrState = errors.New("not allowed in this state")

// PodName returns the pod name of a sandbox.
func PodName(sandboxID string) string { return "sbx-" + sandboxID }

// WorkContainer returns the name of a sandbox's workspace container.
func WorkContainer(sandboxID string) string { return "sbx-" + sandboxID + "-work" }

// CodeVolume returns the name of a sandbox's /workspace volume.
func CodeVolume(sandboxID string) string { return "sbx-" + sandboxID + "-code" }

// HomeVolume returns the name of a project's shared /home/agent volume.
func HomeVolume(projectID string) string { return "proj-" + projectID + "-home" }

// ProjectImage returns the image a project's sandboxes run. For a registry
// project that is its image reference. For a Containerfile project it is
// localhost/wsm-<project id>:<first 12 hex characters of the SHA-256 of the
// Containerfile>, so a changed Containerfile gets a new tag (ADR 0008).
func ProjectImage(p db.Project) string {
	if p.ImageSource != db.ImageSourceContainerfile {
		return p.ImageRef
	}
	sum := sha256.Sum256([]byte(p.Containerfile))
	return "localhost/wsm-" + p.ID + ":" + hex.EncodeToString(sum[:])[:12]
}

// UserAdmin creates and removes project users; wsm-helper does the work.
type UserAdmin interface {
	CreateProjectUser(ctx context.Context, uid int) error
	DeleteProjectUser(ctx context.Context, uid int) error
}

// Mirrors keeps the projects' git mirrors on the host (gitmgr.Mirrors).
type Mirrors interface {
	Update(ctx context.Context, r gitmgr.Repo) error
	Bundle(ctx context.Context, projectID, branch string, w io.Writer) error
	Remove(projectID string) error
}

// Manager runs project and sandbox operations. Operations that change
// state run one at a time.
type Manager struct {
	Store *db.Store
	Users UserAdmin
	// Podman returns a client for the Podman socket of a project UID.
	Podman func(uid int) *podman.Client
	// Secrets seals deploy keys at rest (ADR 0009). Projects with a
	// repository need it.
	Secrets *secret.Box
	// Mirrors holds the git mirrors of projects with a repository.
	Mirrors Mirrors
	Logf    func(format string, args ...any)

	mu sync.Mutex
}

func (m *Manager) logf(format string, args ...any) {
	if m.Logf != nil {
		m.Logf(format, args...)
	}
}

// ProjectSpec is the input for a new project. It has exactly one image
// source: a registry image in ImageRef, or a Containerfile.
type ProjectSpec struct {
	Slug          string
	Name          string
	ImageRef      string
	Containerfile string
	CPUMillis     int64
	MemBytes      int64
	PidsLimit     int64
	// RepoURL is an SSH repository, optional. BaseBranch defaults to
	// gitmgr.DefaultBranch and needs a RepoURL.
	RepoURL    string
	BaseBranch string
}

// ValidSlug reports whether s is a valid project or sandbox slug.
func ValidSlug(s string) bool { return slugPattern.MatchString(s) }

func (s *ProjectSpec) validate() error {
	if s.CPUMillis == 0 {
		s.CPUMillis = DefaultCPUMillis
	}
	if s.MemBytes == 0 {
		s.MemBytes = DefaultMemBytes
	}
	if s.PidsLimit == 0 {
		s.PidsLimit = DefaultPids
	}
	if s.Name == "" {
		s.Name = s.Slug
	}
	switch {
	case !ValidSlug(s.Slug):
		return fmt.Errorf("%w: slug %q: use 1-32 lowercase letters, digits and dashes, starting with a letter", ErrInvalid, s.Slug)
	case len(s.Name) > 100:
		return fmt.Errorf("%w: name is longer than 100 bytes", ErrInvalid)
	case s.ImageRef != "" && s.Containerfile != "":
		return fmt.Errorf("%w: give an image or a containerfile, not both", ErrInvalid)
	case s.ImageRef == "" && s.Containerfile == "":
		return fmt.Errorf("%w: give an image or a containerfile", ErrInvalid)
	case s.Containerfile == "" && !imagePattern.MatchString(s.ImageRef):
		return fmt.Errorf("%w: image %q", ErrInvalid, s.ImageRef)
	case s.CPUMillis < MinCPUMillis || s.CPUMillis > MaxCPUMillis:
		return fmt.Errorf("%w: cpu %d millicores is outside %d-%d", ErrInvalid, s.CPUMillis, MinCPUMillis, MaxCPUMillis)
	case s.MemBytes < MinMemBytes || s.MemBytes > MaxMemBytes:
		return fmt.Errorf("%w: memory %d bytes is outside %d-%d", ErrInvalid, s.MemBytes, int64(MinMemBytes), int64(MaxMemBytes))
	case s.PidsLimit < MinPids || s.PidsLimit > MaxPids:
		return fmt.Errorf("%w: pids %d is outside %d-%d", ErrInvalid, s.PidsLimit, MinPids, MaxPids)
	}
	for _, r := range s.Name {
		if r < 0x20 || r == 0x7f {
			return fmt.Errorf("%w: name contains control characters", ErrInvalid)
		}
	}
	if s.RepoURL == "" {
		if s.BaseBranch != "" {
			return fmt.Errorf("%w: a branch needs a repository", ErrInvalid)
		}
	} else {
		if s.BaseBranch == "" {
			s.BaseBranch = gitmgr.DefaultBranch
		}
		if err := gitmgr.ValidRepoURL(s.RepoURL); err != nil {
			return fmt.Errorf("%w: %v", ErrInvalid, err)
		}
		if err := gitmgr.ValidBranch(s.BaseBranch); err != nil {
			return fmt.Errorf("%w: %v", ErrInvalid, err)
		}
	}
	if s.Containerfile != "" {
		return db.ValidContainerfile(s.Containerfile)
	}
	return nil
}

// SandboxBranch returns the branch a sandbox works on.
func SandboxBranch(sandboxSlug string) string { return "agent/" + sandboxSlug }

// imageSource returns the database's image source for the spec.
func (s *ProjectSpec) imageSource() string {
	if s.Containerfile != "" {
		return db.ImageSourceContainerfile
	}
	return db.ImageSourceRegistry
}

// job records an operation in the jobs table.
func (m *Manager) job(ctx context.Context, typ, ref string, fn func() error) error {
	id, jerr := m.Store.StartJob(ctx, typ, ref)
	if jerr != nil {
		m.logf("record job %s %s: %v", typ, ref, jerr)
	}
	err := fn()
	if jerr == nil {
		// The job outcome is recorded even when the request was cancelled.
		if ferr := m.Store.FinishJob(context.WithoutCancel(ctx), id, err); ferr != nil {
			m.logf("finish job %s %s: %v", typ, ref, ferr)
		}
	}
	return err
}

// CreateProject stores a project and creates its Linux user. When the user
// cannot be created, the project is rolled back. A project with a
// repository gets a new deploy key; wsm does not contact the repository
// until the first sandbox starts.
func (m *Manager) CreateProject(ctx context.Context, spec ProjectSpec, userName func(uid int) string) (db.Project, error) {
	if err := spec.validate(); err != nil {
		return db.Project{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	p := db.Project{
		Slug: spec.Slug, Name: spec.Name, ImageSource: spec.imageSource(),
		ImageRef: spec.ImageRef, Containerfile: spec.Containerfile,
		CPUMillis: spec.CPUMillis, MemBytes: spec.MemBytes, PidsLimit: spec.PidsLimit,
	}
	if spec.RepoURL != "" {
		if err := m.newDeployKey(&p, spec); err != nil {
			return db.Project{}, err
		}
	}
	if err := m.Store.CreateProject(ctx, &p, userName); err != nil {
		return p, err
	}
	err := m.job(ctx, "project.create", p.ID, func() error {
		return m.Users.CreateProjectUser(ctx, p.LinuxUID)
	})
	if err != nil {
		bg := context.WithoutCancel(ctx)
		if derr := m.Users.DeleteProjectUser(bg, p.LinuxUID); derr != nil {
			m.logf("roll back user of project %s: %v", p.Slug, derr)
		}
		if derr := m.Store.DeleteProject(bg, p.ID); derr != nil {
			m.logf("roll back project %s: %v", p.Slug, derr)
		}
		return p, fmt.Errorf("create project %s: %w", p.Slug, err)
	}
	m.logf("project %s created with uid %d", p.Slug, p.LinuxUID)
	return p, nil
}

// newDeployKey gives p a fresh ID and a deploy key sealed for that ID.
func (m *Manager) newDeployKey(p *db.Project, spec ProjectSpec) error {
	if m.Secrets == nil {
		return errors.New("no secret key: cannot store a deploy key")
	}
	id, err := db.NewID()
	if err != nil {
		return err
	}
	key, err := gitmgr.NewDeployKey("wsm-" + spec.Slug)
	if err != nil {
		return err
	}
	sealed, err := m.Secrets.Seal(key.Private, []byte(id))
	clear(key.Private)
	if err != nil {
		return fmt.Errorf("seal deploy key: %w", err)
	}
	p.ID, p.RepoURL, p.BaseBranch = id, spec.RepoURL, spec.BaseBranch
	p.DeployKeyPub, p.DeployKeySealed = key.Public, sealed
	return nil
}

// DeployKey returns the public deploy key of a project, which the owner
// adds to the repository. The private key never leaves the manager.
func (m *Manager) DeployKey(ctx context.Context, projectSlug string) (string, error) {
	p, err := m.Store.ProjectBySlug(ctx, projectSlug)
	if err != nil {
		return "", err
	}
	if !p.HasRepo() {
		return "", fmt.Errorf("project %s has no repository: %w", projectSlug, db.ErrNotFound)
	}
	return p.DeployKeyPub, nil
}

// DeleteProject removes every sandbox of the project, its Podman state, its
// Linux user and its files, then the project itself.
func (m *Manager) DeleteProject(ctx context.Context, slug string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	p, err := m.Store.ProjectBySlug(ctx, slug)
	if err != nil {
		return err
	}
	return m.job(ctx, "project.delete", p.ID, func() error {
		sbs, err := m.Store.Sandboxes(ctx, p.ID)
		if err != nil {
			return err
		}
		pc := m.Podman(p.LinuxUID)
		defer pc.Close()
		// Podman errors are logged, not fatal: deleting the user below
		// resets the user's Podman and removes its storage anyway.
		for _, sb := range sbs {
			if err := m.removePodman(ctx, pc, sb); err != nil {
				m.logf("delete project %s: sandbox %s: %v", p.Slug, sb.Slug, err)
			}
		}
		if err := pc.VolumeRemove(ctx, HomeVolume(p.ID)); err != nil {
			m.logf("delete project %s: home volume: %v", p.Slug, err)
		}
		if err := m.Users.DeleteProjectUser(ctx, p.LinuxUID); err != nil {
			return fmt.Errorf("delete project %s: %w", p.Slug, err)
		}
		if p.HasRepo() && m.Mirrors != nil {
			if err := m.Mirrors.Remove(p.ID); err != nil {
				return fmt.Errorf("delete project %s: %w", p.Slug, err)
			}
		}
		for _, sb := range sbs {
			if err := m.Store.DeleteSandbox(ctx, sb.ID); err != nil {
				return err
			}
		}
		if err := m.Store.DeleteProject(ctx, p.ID); err != nil {
			return err
		}
		m.logf("project %s deleted", p.Slug)
		return nil
	})
}

// Projects lists all projects.
func (m *Manager) Projects(ctx context.Context) ([]db.Project, error) {
	return m.Store.Projects(ctx)
}

// Sandboxes lists the sandboxes of a project, or of all projects when
// projectSlug is empty.
func (m *Manager) Sandboxes(ctx context.Context, projectSlug string) ([]db.Sandbox, error) {
	if projectSlug == "" {
		return m.Store.Sandboxes(ctx, "")
	}
	p, err := m.Store.ProjectBySlug(ctx, projectSlug)
	if err != nil {
		return nil, err
	}
	return m.Store.Sandboxes(ctx, p.ID)
}

// Lookup returns a project and one of its sandboxes by slug.
func (m *Manager) Lookup(ctx context.Context, projectSlug, sandboxSlug string) (db.Project, db.Sandbox, error) {
	p, err := m.Store.ProjectBySlug(ctx, projectSlug)
	if err != nil {
		return p, db.Sandbox{}, err
	}
	sb, err := m.Store.SandboxBySlug(ctx, p.ID, sandboxSlug)
	return p, sb, err
}

// workCommand returns the command for the workspace container: the image's
// own CMD, or "sleep infinity" when the image has neither CMD nor
// ENTRYPOINT (plan section 3.4).
func workCommand(cfg podman.ImageConfig) []string {
	if len(cfg.Cmd) == 0 && len(cfg.Entrypoint) == 0 {
		return []string{"sleep", "infinity"}
	}
	return nil
}

// WorkSpec returns the workspace container for a sandbox, with the
// hardening of CLAUDE.md invariant 9. The only volumes are the sandbox's
// code volume and the project's home volume; there are no host mounts.
func WorkSpec(p db.Project, sb db.Sandbox, cmd []string) podman.ContainerSpec {
	return podman.ContainerSpec{
		Name:            WorkContainer(sb.ID),
		Image:           ProjectImage(p),
		Pod:             PodName(sb.ID),
		Labels:          labels(p, sb),
		User:            strconv.Itoa(ContainerUID) + ":" + strconv.Itoa(ContainerGID),
		Command:         cmd,
		Env:             workEnv(p, sb),
		WorkDir:         WorkspaceDir,
		Init:            true,
		CapDrop:         []string{"ALL"},
		NoNewPrivileges: true,
		Resources: &podman.Resources{
			CPU:    &podman.CPULimit{Quota: p.CPUMillis * cpuPeriod / 1000, Period: cpuPeriod},
			Memory: &podman.MemoryLimit{Limit: p.MemBytes},
			Pids:   &podman.PidsLimit{Limit: p.PidsLimit},
		},
		Volumes: []podman.NamedVolume{
			{Name: CodeVolume(sb.ID), Dest: WorkspaceDir},
			{Name: HomeVolume(p.ID), Dest: HomeDir},
		},
	}
}

// workEnv is the workspace container's environment (plan section 3.4).
func workEnv(p db.Project, sb db.Sandbox) map[string]string {
	env := map[string]string{
		"WSM_PROJECT": p.Slug,
		"WSM_SANDBOX": sb.Slug,
		"HOME":        HomeDir,
	}
	if sb.Branch != "" {
		env["WSM_BRANCH"] = sb.Branch
	}
	return env
}

// PodSpec returns the pod for a sandbox: pasta networking and a keep-id
// user namespace at pod level that maps ContainerUID to the project user.
func PodSpec(p db.Project, sb db.Sandbox) podman.PodSpec {
	return podman.PodSpec{
		Name:     PodName(sb.ID),
		Labels:   labels(p, sb),
		Userns:   &podman.Namespace{Mode: "keep-id", Value: fmt.Sprintf("uid=%d,gid=%d", ContainerUID, ContainerGID)},
		Netns:    &podman.Namespace{Mode: "pasta"},
		Hostname: sb.Slug,
	}
}

func labels(p db.Project, sb db.Sandbox) map[string]string {
	return map[string]string{"wsm.project": p.ID, "wsm.sandbox": sb.ID}
}

// CreateSandbox stores a sandbox and creates its pod, volumes and workspace
// container. The sandbox is not started. On failure everything is rolled
// back.
func (m *Manager) CreateSandbox(ctx context.Context, projectSlug, slug string) (db.Sandbox, error) {
	if !ValidSlug(slug) {
		return db.Sandbox{}, fmt.Errorf("%w: slug %q: use 1-32 lowercase letters, digits and dashes, starting with a letter", ErrInvalid, slug)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	p, err := m.Store.ProjectBySlug(ctx, projectSlug)
	if err != nil {
		return db.Sandbox{}, err
	}
	sb := db.Sandbox{ProjectID: p.ID, Slug: slug, Status: db.StatusCreated}
	if p.HasRepo() {
		sb.Branch = SandboxBranch(slug)
	}
	if err := m.Store.CreateSandbox(ctx, &sb, PodName); err != nil {
		return sb, err
	}
	pc := m.Podman(p.LinuxUID)
	defer pc.Close()
	err = m.job(ctx, "sandbox.create", sb.ID, func() error {
		return m.createPodman(ctx, pc, p, sb)
	})
	if err != nil {
		bg := context.WithoutCancel(ctx)
		if rerr := m.removePodman(bg, pc, sb); rerr != nil {
			m.logf("roll back sandbox %s/%s: %v", p.Slug, slug, rerr)
		}
		if derr := m.Store.DeleteSandbox(bg, sb.ID); derr != nil {
			m.logf("roll back sandbox %s/%s: %v", p.Slug, slug, derr)
		}
		return sb, fmt.Errorf("create sandbox %s/%s: %w", p.Slug, slug, err)
	}
	m.logf("sandbox %s/%s created as %s", p.Slug, slug, sb.PodName)
	return sb, nil
}

func (m *Manager) createPodman(ctx context.Context, pc *podman.Client, p db.Project, sb db.Sandbox) error {
	if err := m.ensureImage(ctx, pc, p); err != nil {
		return err
	}
	cfg, err := pc.ImageInspect(ctx, ProjectImage(p))
	if err != nil {
		return err
	}
	if err := pc.VolumeCreate(ctx, HomeVolume(p.ID), map[string]string{"wsm.project": p.ID}); err != nil {
		return err
	}
	if err := pc.VolumeCreate(ctx, CodeVolume(sb.ID), labels(p, sb)); err != nil {
		return err
	}
	if _, err := pc.PodCreate(ctx, PodSpec(p, sb)); err != nil {
		return err
	}
	if _, err := pc.ContainerCreate(ctx, WorkSpec(p, sb, workCommand(cfg))); err != nil {
		return err
	}
	return nil
}

// ensureImage makes sure the project's image is present for the project
// user: a missing registry image is pulled, a missing Containerfile image
// is built. A present image is used as it is.
func (m *Manager) ensureImage(ctx context.Context, pc *podman.Client, p db.Project) error {
	ref := ProjectImage(p)
	ok, err := pc.ImageExists(ctx, ref)
	if err != nil || ok {
		return err
	}
	m.logf("project %s: image %s is missing, getting it", p.Slug, ref)
	_, err = m.getImage(ctx, pc, p, podman.PullMissing)
	return err
}

// getImage pulls (with policy) or builds the project's image as the
// project user and returns its ID.
func (m *Manager) getImage(ctx context.Context, pc *podman.Client, p db.Project, policy string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, imageTimeout)
	defer cancel()
	ref := ProjectImage(p)
	if p.ImageSource == db.ImageSourceContainerfile {
		return pc.ImageBuild(ctx, ref, p.Containerfile)
	}
	if err := pc.ImagePull(ctx, ref, policy); err != nil {
		return "", err
	}
	return pc.ImageID(ctx, ref)
}

// RefreshImage builds a Containerfile project's image, or pulls a registry
// project's image again, now. It returns the image reference and ID.
// Existing sandboxes keep the image they were created with; new sandboxes
// get the new one.
func (m *Manager) RefreshImage(ctx context.Context, projectSlug string) (ref, id string, err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	p, err := m.Store.ProjectBySlug(ctx, projectSlug)
	if err != nil {
		return "", "", err
	}
	pc := m.Podman(p.LinuxUID)
	defer pc.Close()
	ref = ProjectImage(p)
	err = m.job(ctx, "project.image", p.ID, func() error {
		var err error
		id, err = m.getImage(ctx, pc, p, podman.PullAlways)
		return err
	})
	if err != nil {
		return ref, "", fmt.Errorf("image of project %s: %w", p.Slug, err)
	}
	m.logf("project %s: image %s is %s", p.Slug, ref, id)
	return ref, id, nil
}

// removePodman removes a sandbox's pod and code volume. Missing objects are
// not errors.
func (m *Manager) removePodman(ctx context.Context, pc *podman.Client, sb db.Sandbox) error {
	if err := pc.PodRemove(ctx, PodName(sb.ID)); err != nil {
		return err
	}
	return pc.VolumeRemove(ctx, CodeVolume(sb.ID))
}

// StartSandbox starts a sandbox's pod. The first start of a sandbox in a
// project with a repository also seeds its code (ADR 0010).
func (m *Manager) StartSandbox(ctx context.Context, projectSlug, slug string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	p, sb, err := m.Lookup(ctx, projectSlug, slug)
	if err != nil {
		return err
	}
	pc := m.Podman(p.LinuxUID)
	defer pc.Close()
	err = m.job(ctx, "sandbox.start", sb.ID, func() error {
		if err := pc.PodStart(ctx, sb.PodName); err != nil {
			return err
		}
		if !p.HasRepo() || sb.SeededAt != nil {
			return nil
		}
		if err := m.seed(ctx, pc, p, sb); err != nil {
			// Without its code the sandbox is of no use. It stops, and
			// the next start tries again.
			bg := context.WithoutCancel(ctx)
			if serr := pc.PodStop(bg, sb.PodName, stopTimeout); serr != nil {
				m.logf("stop sandbox %s/%s after a failed seed: %v", p.Slug, sb.Slug, serr)
			}
			return fmt.Errorf("start sandbox %s/%s: %w", p.Slug, sb.Slug, err)
		}
		return nil
	})
	status := db.StatusRunning
	if err != nil {
		status = db.StatusFailed
	}
	if serr := m.Store.SetSandboxStatus(context.WithoutCancel(ctx), sb.ID, status); serr != nil && err == nil {
		err = serr
	}
	return err
}

// StopSandbox stops a sandbox's pod.
func (m *Manager) StopSandbox(ctx context.Context, projectSlug, slug string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	p, sb, err := m.Lookup(ctx, projectSlug, slug)
	if err != nil {
		return err
	}
	pc := m.Podman(p.LinuxUID)
	defer pc.Close()
	err = m.job(ctx, "sandbox.stop", sb.ID, func() error {
		return pc.PodStop(ctx, sb.PodName, stopTimeout)
	})
	if err != nil {
		return err
	}
	return m.Store.SetSandboxStatus(ctx, sb.ID, db.StatusStopped)
}

// DeleteSandbox removes a sandbox's pod, its code volume and its row. The
// project's home volume stays.
func (m *Manager) DeleteSandbox(ctx context.Context, projectSlug, slug string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	p, sb, err := m.Lookup(ctx, projectSlug, slug)
	if err != nil {
		return err
	}
	pc := m.Podman(p.LinuxUID)
	defer pc.Close()
	err = m.job(ctx, "sandbox.delete", sb.ID, func() error {
		return m.removePodman(ctx, pc, sb)
	})
	if err != nil {
		return err
	}
	return m.Store.DeleteSandbox(ctx, sb.ID)
}

// ShellTarget returns a Podman client and the workspace container of a
// running sandbox, for a terminal session. The caller closes the client.
func (m *Manager) ShellTarget(ctx context.Context, projectSlug, slug string) (*podman.Client, string, error) {
	p, sb, err := m.Lookup(ctx, projectSlug, slug)
	if err != nil {
		return nil, "", err
	}
	if sb.Status != db.StatusRunning {
		return nil, "", fmt.Errorf("sandbox %s/%s is %s: %w", projectSlug, slug, sb.Status, ErrState)
	}
	if err := m.Store.TouchSandbox(ctx, sb.ID); err != nil {
		m.logf("touch sandbox %s: %v", sb.ID, err)
	}
	return m.Podman(p.LinuxUID), WorkContainer(sb.ID), nil
}
