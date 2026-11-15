package main

import (
	"fmt"
	"strings"
	"text/tabwriter"

	"github.com/JCombee/wsm/internal/api"
)

// stringList is a flag that may be given more than once.
type stringList []string

func (l *stringList) String() string { return strings.Join(*l, ",") }

func (l *stringList) Set(v string) error {
	*l = append(*l, v)
	return nil
}

// runProjectBlock implements "wsm project block list|set|remove".
func runProjectBlock(e env, c *api.Client, args []string) error {
	if len(args) == 0 {
		return usageError{"missing block subcommand"}
	}
	switch sub, args := args[0], args[1:]; sub {
	case "list":
		if len(args) != 1 {
			return usageError{"want exactly one project"}
		}
		bs, err := c.Blocks(e.ctx, args[0])
		if err != nil {
			return err
		}
		tw := tabwriter.NewWriter(e.stdout, 0, 4, 2, ' ', 0)
		fmt.Fprintln(tw, "BLOCK\tPROFILE\tTAGS")
		for _, b := range bs {
			fmt.Fprintf(tw, "%s\t%s\t%s\n", b.Block, b.Profile, strings.Join(b.Tags, ","))
		}
		return tw.Flush()
	case "set":
		// SLUG and BLOCK come first, then the flags.
		if len(args) < 2 || strings.HasPrefix(args[0], "-") || strings.HasPrefix(args[1], "-") {
			return usageError{"want a project and a block before the flags"}
		}
		project, block := args[0], args[1]
		fs := newFlags("project block set")
		var tags stringList
		fs.Var(&tags, "tag", "allowed tag (version); repeat for more, the first is the default")
		profile := fs.String("profile", "", "profile (default: the block's default)")
		if err := fs.Parse(args[2:]); err != nil {
			return usageError{err.Error()}
		}
		if fs.NArg() != 0 {
			return usageError{"unexpected arguments"}
		}
		if len(tags) == 0 {
			return usageError{"give at least one --tag"}
		}
		b, err := c.SetBlock(e.ctx, project, block, api.SetBlockRequest{Profile: *profile, Tags: tags})
		if err != nil {
			return err
		}
		fmt.Fprintf(e.stdout, "project %s block %s set: profile %s, tags %s\n", project, b.Block, b.Profile,
			strings.Join(b.Tags, ","))
		return nil
	case "remove":
		if len(args) != 2 {
			return usageError{"want a project and a block"}
		}
		if err := c.RemoveBlock(e.ctx, args[0], args[1]); err != nil {
			return err
		}
		fmt.Fprintf(e.stdout, "project %s block %s removed\n", args[0], args[1])
		return nil
	default:
		return usageError{fmt.Sprintf("unknown block subcommand %q", sub)}
	}
}

// runService implements "wsm service list".
func runService(e env) error {
	if len(e.args) != 1 || e.args[0] != "list" {
		return usageError{"want list"}
	}
	svcs, err := apiClient(e).Services(e.ctx)
	if err != nil {
		return err
	}
	tw := tabwriter.NewWriter(e.stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "BLOCK\tTAG\tADDRESS\tSTATE\tVERSION")
	for _, s := range svcs {
		v := s.Version
		if v == "" {
			v = "-"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", s.Block, s.Tag, s.Address, s.State, v)
	}
	return tw.Flush()
}
