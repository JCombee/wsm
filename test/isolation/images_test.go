//go:build isolation

package isolation

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/JCombee/wsm/internal/helper"
)

// M2a: project images are built from a Containerfile or pulled from a
// public registry, as the project user (ADR 0008).

// minimalContainerfile is a workspace image without packages beyond the
// base image, so it builds quickly.
const minimalContainerfile = `FROM docker.io/library/debian:trixie-slim
RUN groupadd --gid 1000 agent \
 && useradd --uid 1000 --gid 1000 --home-dir /home/agent --create-home --shell /bin/bash agent \
 && install -d -o 1000 -g 1000 /workspace
WORKDIR /workspace
CMD ["sleep", "infinity"]
`

// registryImage is the public image of the registry tests.
const registryImage = "docker.io/library/debian:trixie-slim"

// ctxImage gives a test time for a pull or build on a slow mirror.
func ctxImage(t *testing.T) context.Context {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	t.Cleanup(cancel)
	return ctx
}

// createBuiltProject creates a project from a Containerfile with the test
// limits, through the CLI.
func createBuiltProject(ctx context.Context, t *testing.T, slug, containerfile string) project {
	t.Helper()
	path := filepath.Join(s.tmp, slug+".Containerfile")
	if err := os.WriteFile(path, []byte(containerfile), 0o600); err != nil {
		t.Fatal(err)
	}
	p, err := createProjectFrom(ctx, s, slug, "--containerfile", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { wsm(context.Background(), "project", "delete", slug) })
	return p
}

var imageLine = regexp.MustCompile(`^project (\S+) image (\S+) id ([0-9a-f]{64})\n$`)

// projectImage runs "wsm project image" and returns the image reference
// and ID it reports.
func projectImage(ctx context.Context, t *testing.T, slug string) (ref, id string) {
	t.Helper()
	out, err := wsm(ctx, "project", "image", slug)
	if err != nil {
		t.Fatal(err)
	}
	m := imageLine.FindStringSubmatch(out)
	if m == nil || m[1] != slug {
		t.Fatalf("wsm project image %s: unexpected output %q", slug, out)
	}
	return m[2], m[3]
}

// userImageID returns the ID of ref in a project user's storage.
func userImageID(ctx context.Context, t *testing.T, p project, ref string) string {
	t.Helper()
	out, err := asUser(ctx, p, "podman", "image", "inspect", "--format", "{{.Id}}", ref)
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(out)
}

// imageDirs returns every directory on the host that holds the metadata of
// image id in some containers storage, root's included.
func imageDirs(ctx context.Context, t *testing.T, id string) []string {
	t.Helper()
	out, err := run(ctx, "find", "/", "(", "-path", "/proc", "-o", "-path", "/sys", ")", "-prune",
		"-o", "-type", "d", "-path", "*/overlay-images/"+id, "-print")
	if err != nil {
		t.Fatal(err)
	}
	return strings.Fields(out)
}

// checkOnlyProjectStorage checks that image id is in p's storage, owned by
// p's user, and not in the storage of root, of wsm or outside the project
// homes.
func checkOnlyProjectStorage(ctx context.Context, t *testing.T, p project, id string) {
	t.Helper()
	dirs := imageDirs(ctx, t, id)
	home := helper.HomeDir(p.uid) + "/"
	found := false
	for _, d := range dirs {
		if !strings.HasPrefix(d, helper.HomeRoot+"/wsm-p-") {
			t.Errorf("image %.12s is stored outside the project users' homes: %s", id, d)
			continue
		}
		if !strings.HasPrefix(d, home) {
			continue // another project built on the same base image
		}
		found = true
		fi, err := os.Stat(d)
		if err != nil {
			t.Fatal(err)
		}
		if uid := fi.Sys().(*syscall.Stat_t).Uid; int(uid) != p.uid {
			t.Errorf("%s is owned by uid %d, want %d", d, uid, p.uid)
		}
	}
	if !found {
		t.Errorf("image %.12s is not in the storage of %s; found %v", id, p.user, dirs)
	}
	// Control on the names: root's Podman does not know the image either.
	if out, err := run(ctx, "podman", "image", "exists", id); err == nil {
		t.Errorf("root's Podman has image %.12s: %s", id, out)
	}
}

func TestBuildRunsAsProjectUser(t *testing.T) {
	ctx := ctxImage(t)
	p := createBuiltProject(ctx, t, "iso-d", minimalContainerfile)
	ref, id := projectImage(ctx, t, p.slug)
	if !strings.HasPrefix(ref, "localhost/wsm-"+p.id+":") {
		t.Errorf("image %s, want localhost/wsm-%s:<hash>", ref, p.id)
	}
	if got := userImageID(ctx, t, p, ref); got != id {
		t.Errorf("%s in the storage of %s is %s, wsm reported %s", ref, p.user, got, id)
	}
	checkOnlyProjectStorage(ctx, t, p, id)
}

// buildProbes are the targets the RUN step of TestBuildNetworkFiltered
// tries, as /dev/tcp paths, with the counter that must block each.
var buildProbes = []struct {
	target  string
	counter string
}{
	{"192.168.0.1/22", "lan_blocked"},
	{"169.254.169.254/80", "linklocal_blocked"},
}

func TestBuildNetworkFiltered(t *testing.T) {
	ctx := ctxImage(t)
	var cf strings.Builder
	cf.WriteString("FROM docker.io/library/debian:trixie-slim\nRUN for t in")
	for _, pr := range buildProbes {
		cf.WriteString(" " + pr.target)
	}
	// The control: the internet is open during the build.
	cf.WriteString(` deb.debian.org/80; do if timeout 5 bash -c "exec 3<>/dev/tcp/$t"; then echo "$t open"; else echo "$t blocked"; fi; done > /probes.txt || true` + "\n")
	p := createBuiltProject(ctx, t, "iso-f", cf.String())

	before := map[string]int64{}
	for _, pr := range buildProbes {
		before[pr.counter] = counter(t, pr.counter)
	}
	ref, _ := projectImage(ctx, t, p.slug)
	after := map[string]int64{}
	for _, pr := range buildProbes {
		after[pr.counter] = counter(t, pr.counter)
	}

	out, err := asUser(ctx, p, "podman", "run", "--rm", "--network", "none", "--cap-drop=ALL",
		"--security-opt=no-new-privileges", "--user", "1000:1000", ref, "cat", "/probes.txt")
	if err != nil {
		t.Fatal(err)
	}
	results := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if target, result, ok := strings.Cut(line, " "); ok {
			results[target] = result
		}
	}
	if results["deb.debian.org/80"] != "open" {
		t.Errorf("control: the build could not reach deb.debian.org:80: %q", out)
	}
	for _, pr := range buildProbes {
		if results[pr.target] != "blocked" {
			t.Errorf("build RUN to %s: %q, want blocked", pr.target, results[pr.target])
		}
		if after[pr.counter] <= before[pr.counter] {
			t.Errorf("build RUN to %s: counter %s did not move (%d -> %d)", pr.target, pr.counter,
				before[pr.counter], after[pr.counter])
		}
	}
}

func TestSandboxOnBuiltImage(t *testing.T) {
	ctx := ctxImage(t)
	g := createBuiltProject(ctx, t, "iso-g", minimalContainerfile)
	ps, err := s.api.Projects(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var ref string
	for _, p := range ps {
		if p.Slug == g.slug {
			ref = p.Image
		}
	}
	if _, err := asUser(ctx, g, "podman", "image", "exists", ref); err == nil {
		t.Fatalf("%s exists before the first sandbox", ref)
	}
	// No "wsm project image": sandbox create builds the missing tag.
	if err := createSandbox(ctx, s, &g); err != nil {
		t.Fatal(err)
	}
	id := userImageID(ctx, t, g, ref)
	checkOnlyProjectStorage(ctx, t, g, id)
	ci := inspectWork(t, g)
	if ci.Image != id {
		t.Errorf("workspace container runs image %s, want the built %s", ci.Image, id)
	}
	checkHardening(ctx, t, g)
}

func TestRegistryImagePulledByProjectUser(t *testing.T) {
	ctx := ctxImage(t)
	e, err := createProjectFrom(ctx, s, "iso-e", "--image", registryImage)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { wsm(context.Background(), "project", "delete", "iso-e") })
	ref, id := projectImage(ctx, t, e.slug)
	if ref != registryImage {
		t.Errorf("image %s, want %s", ref, registryImage)
	}
	if got := userImageID(ctx, t, e, registryImage); got != id {
		t.Errorf("%s in the storage of %s is %s, wsm reported %s", registryImage, e.user, got, id)
	}
	checkOnlyProjectStorage(ctx, t, e, id)
}

func TestManagerStateNotReachable(t *testing.T) {
	ctx := ctxT(t)
	const dir = "/var/lib/wsm"
	db := filepath.Join(dir, "wsm.db")
	// Control: the state exists and root reads it.
	if _, err := os.ReadDir(dir); err != nil {
		t.Fatalf("control: %v", err)
	}
	if _, err := os.ReadFile(db); err != nil {
		t.Fatalf("control: %v", err)
	}
	for _, p := range []project{s.a, s.b} {
		for _, spec := range []string{"readdir:" + dir, "readfile:" + db} {
			if ok, out := probeAs(ctx, p.user, spec); ok {
				t.Errorf("%s: %s", p.user, out)
			}
		}
		if out, err := inSandbox(ctx, p, "test", "-e", dir); err == nil {
			t.Errorf("%s: %s exists in the sandbox %s", p.slug, dir, out)
		}
	}
}
