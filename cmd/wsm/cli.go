package main

import (
	"flag"
	"fmt"
	"io"
	"math"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/JCombee/wsm/internal/api"
	"github.com/JCombee/wsm/internal/db"
	"github.com/JCombee/wsm/internal/terminal"
)

// socketEnv overrides the API socket for the CLI.
const socketEnv = "WSM_API_SOCKET"

func apiClient(e env) *api.Client {
	path := api.DefaultSocket
	if e.getenv != nil {
		if s := e.getenv(socketEnv); s != "" {
			path = s
		}
	}
	return api.NewClient(path)
}

// splitTarget parses "<project>/<sandbox>".
func splitTarget(s string) (project, sandbox string, err error) {
	project, sandbox, ok := strings.Cut(s, "/")
	if !ok || project == "" || sandbox == "" || strings.Contains(sandbox, "/") {
		return "", "", usageError{fmt.Sprintf("%q is not <project>/<sandbox>", s)}
	}
	return project, sandbox, nil
}

// parseCPUs turns a CPU count such as "1.5" into millicores.
func parseCPUs(s string) (int64, error) {
	f, err := strconv.ParseFloat(s, 64)
	if err != nil || f <= 0 || f > 1000 || math.IsNaN(f) {
		return 0, fmt.Errorf("invalid cpu count %q", s)
	}
	return int64(math.Round(f * 1000)), nil
}

// parseBytes turns a size such as "512m" or "2g" into bytes.
func parseBytes(s string) (int64, error) {
	mult := int64(1)
	num := strings.ToLower(s)
	for suffix, m := range map[string]int64{"k": 1 << 10, "m": 1 << 20, "g": 1 << 30} {
		if strings.HasSuffix(num, suffix) {
			mult, num = m, strings.TrimSuffix(num, suffix)
			break
		}
	}
	n, err := strconv.ParseInt(num, 10, 64)
	if err != nil || n <= 0 || n > math.MaxInt64/mult {
		return 0, fmt.Errorf("invalid size %q", s)
	}
	return n * mult, nil
}

// readContainerfile reads a local Containerfile of at most
// db.MaxContainerfile bytes. The manager checks the content and stores it
// in its database only.
func readContainerfile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("containerfile: %w", err)
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, db.MaxContainerfile+1))
	if err != nil {
		return "", fmt.Errorf("containerfile %s: %w", path, err)
	}
	if len(data) > db.MaxContainerfile {
		return "", fmt.Errorf("containerfile %s is larger than %d bytes", path, db.MaxContainerfile)
	}
	return string(data), nil
}

func newFlags(name string) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	return fs
}

func runProject(e env) error {
	if len(e.args) == 0 {
		return usageError{"missing project subcommand"}
	}
	c := apiClient(e)
	switch sub, args := e.args[0], e.args[1:]; sub {
	case "create":
		fs := newFlags("project create")
		slug := fs.String("slug", "", "project slug")
		name := fs.String("name", "", "display name (default: the slug)")
		image := fs.String("image", "", "workspace image from a public registry")
		containerfile := fs.String("containerfile", "", "local Containerfile to build the workspace image from")
		cpus := fs.String("cpus", "", "CPU limit per sandbox, e.g. 1.5")
		memory := fs.String("memory", "", "memory limit per sandbox, e.g. 2g")
		pids := fs.Int64("pids", 0, "process limit per sandbox")
		repo := fs.String("repo", "", "git repository over SSH: ssh://user@host[:port]/path or user@host:path")
		branch := fs.String("branch", "", "base branch of the repository (default main)")
		if err := fs.Parse(args); err != nil {
			return usageError{err.Error()}
		}
		if fs.NArg() != 0 || *slug == "" || (*image == "") == (*containerfile == "") {
			return usageError{"--slug and exactly one of --image and --containerfile are required"}
		}
		if *branch != "" && *repo == "" {
			return usageError{"--branch needs --repo"}
		}
		req := api.CreateProjectRequest{Slug: *slug, Name: *name, Image: *image, PidsLimit: *pids,
			RepoURL: *repo, BaseBranch: *branch}
		var err error
		if *containerfile != "" {
			if req.Containerfile, err = readContainerfile(*containerfile); err != nil {
				return err
			}
		}
		if *cpus != "" {
			if req.CPUMillis, err = parseCPUs(*cpus); err != nil {
				return usageError{err.Error()}
			}
		}
		if *memory != "" {
			if req.MemBytes, err = parseBytes(*memory); err != nil {
				return usageError{err.Error()}
			}
		}
		p, err := c.CreateProject(e.ctx, req)
		if err != nil {
			return err
		}
		fmt.Fprintf(e.stdout, "project %s created (user %s, uid %d, image %s)\n", p.Slug, p.LinuxUser, p.LinuxUID, p.Image)
		if p.RepoURL != "" {
			fmt.Fprintf(e.stdout, "repository %s, base branch %s\n", p.RepoURL, p.BaseBranch)
			fmt.Fprintf(e.stdout, "add the deploy key from \"wsm project key %s\" to the repository before the first sandbox starts\n", p.Slug)
		}
		return nil
	case "key":
		if len(args) != 1 {
			return usageError{"want exactly one project"}
		}
		k, err := c.DeployKey(e.ctx, args[0])
		if err != nil {
			return err
		}
		fmt.Fprintln(e.stdout, k.PublicKey)
		return nil
	case "image":
		if len(args) != 1 {
			return usageError{"want exactly one project"}
		}
		img, err := c.ProjectImage(e.ctx, args[0])
		if err != nil {
			return err
		}
		fmt.Fprintf(e.stdout, "project %s image %s id %s\n", args[0], img.Image, img.ID)
		return nil
	case "list":
		if len(args) != 0 {
			return usageError{"unexpected arguments"}
		}
		ps, err := c.Projects(e.ctx)
		if err != nil {
			return err
		}
		tw := tabwriter.NewWriter(e.stdout, 0, 4, 2, ' ', 0)
		fmt.Fprintln(tw, "SLUG\tUSER\tSOURCE\tIMAGE\tCPUS\tMEMORY\tPIDS")
		for _, p := range ps {
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%.2f\t%dMiB\t%d\n", p.Slug, p.LinuxUser, p.ImageSource, p.Image,
				float64(p.CPUMillis)/1000, p.MemBytes>>20, p.PidsLimit)
		}
		return tw.Flush()
	case "delete":
		if len(args) != 1 {
			return usageError{"want exactly one project"}
		}
		if err := c.DeleteProject(e.ctx, args[0]); err != nil {
			return err
		}
		fmt.Fprintf(e.stdout, "project %s deleted\n", args[0])
		return nil
	default:
		return usageError{fmt.Sprintf("unknown project subcommand %q", sub)}
	}
}

func runSandbox(e env) error {
	if len(e.args) == 0 {
		return usageError{"missing sandbox subcommand"}
	}
	c := apiClient(e)
	sub, args := e.args[0], e.args[1:]
	if sub == "list" {
		if len(args) > 1 {
			return usageError{"unexpected arguments"}
		}
		project := ""
		if len(args) == 1 {
			project = args[0]
		}
		sbs, err := c.Sandboxes(e.ctx, project)
		if err != nil {
			return err
		}
		tw := tabwriter.NewWriter(e.stdout, 0, 4, 2, ' ', 0)
		fmt.Fprintln(tw, "SANDBOX\tSTATUS\tPOD\tCREATED")
		for _, sb := range sbs {
			fmt.Fprintf(tw, "%s/%s\t%s\t%s\t%s\n", sb.Project, sb.Slug, sb.Status, sb.Pod,
				sb.CreatedAt.Local().Format(time.DateTime))
		}
		return tw.Flush()
	}
	if len(args) != 1 {
		return usageError{"want exactly one <project>/<sandbox>"}
	}
	project, sandbox, err := splitTarget(args[0])
	if err != nil {
		return err
	}
	var verb string
	switch sub {
	case "create":
		_, err = c.CreateSandbox(e.ctx, project, sandbox)
		verb = "created"
	case "start":
		err, verb = c.StartSandbox(e.ctx, project, sandbox), "started"
	case "stop":
		err, verb = c.StopSandbox(e.ctx, project, sandbox), "stopped"
	case "delete":
		err, verb = c.DeleteSandbox(e.ctx, project, sandbox), "deleted"
	default:
		return usageError{fmt.Sprintf("unknown sandbox subcommand %q", sub)}
	}
	if err != nil {
		return err
	}
	fmt.Fprintf(e.stdout, "sandbox %s/%s %s\n", project, sandbox, verb)
	return nil
}

// runShell attaches the terminal to tmux in a running sandbox.
func runShell(e env) error {
	if len(e.args) != 1 {
		return usageError{"want exactly one <project>/<sandbox>"}
	}
	project, sandbox, err := splitTarget(e.args[0])
	if err != nil {
		return err
	}
	in, out := os.Stdin, os.Stdout
	inFd, outFd := int(in.Fd()), int(out.Fd())
	rows, cols, ok := termSize(outFd)
	if !ok {
		rows, cols = 24, 80
	}
	ws, err := apiClient(e).Terminal(e.ctx, project, sandbox, rows, cols)
	if err != nil {
		return err
	}
	defer ws.CloseNow()

	if isTerminal(inFd) {
		old, err := makeRaw(inFd)
		if err != nil {
			return fmt.Errorf("raw terminal: %w", err)
		}
		defer restore(inFd, old)
	}
	resize := make(chan [2]int, 1)
	winch := make(chan os.Signal, 1)
	signal.Notify(winch, syscall.SIGWINCH)
	defer signal.Stop(winch)
	go func() {
		for range winch {
			if r, c, ok := termSize(outFd); ok {
				select {
				case resize <- [2]int{r, c}:
				default:
				}
			}
		}
	}()
	return terminal.Attach(e.ctx, ws, in, out, resize)
}
