// Package cli implements a tiny, dependency-free subcommand dispatcher used by
// all ipcod binaries. It supports nested commands, per-command flag sets, an
// auto-generated help screen, and default-to-first-command behavior.
//
// Keeping this in-tree (rather than pulling in cobra) keeps the agent and
// beacon static binaries small and dependency-free.
package cli

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"sort"
	"strings"
)

// ErrHelp is returned by a Command.Run when the user asked for help. The
// dispatcher treats it as a successful (0) exit after help has been printed.
var ErrHelp = errors.New("cli: help requested")

// Command is one node in the command tree. A Command with a non-nil Run is
// runnable; if it also has Subcommands, Run acts as the default when no
// subcommand is given (unless DefaultDisabled is set).
type Command struct {
	// Name is the command word used to invoke it (e.g. "agent").
	Name string
	// Short is a one-line description shown in the parent's help listing.
	Short string
	// Long is the detailed description shown at the top of this command's help.
	Long string
	// Sub is the list of child commands, may be nil for a leaf command.
	Sub []*Command
	// Run executes the command. args are the arguments after the command name
	// (flags). Returning ErrHelp signals a clean help exit.
	Run func(cmd *Command, args []string) error
	// DefaultDisabled prevents Run from being used as the implicit default
	// when the command has subcommands.
	DefaultDisabled bool
	// Hidden keeps the command out of help listings.
	Hidden bool
	// flagSetup registers command-specific flags on the flag set. It is called
	// before Run and is where a command binds its options.
	flagSetup func(fs *flag.FlagSet)
}

// app ties a root command to an output writer for errors and tracks the
// invocation path (e.g. ["ipcod","agent","check"]) as dispatch descends, so
// help and error messages show the full command path.
type app struct {
	root *Command
	errw io.Writer
	path []string // invocation path, root name first
}

// Execute runs the command tree against args (typically os.Args[1:]) and
// returns the process exit code.
func Execute(root *Command, args []string, errw io.Writer) int {
	a := &app{root: root, errw: errw, path: []string{root.Name}}
	code, err := a.dispatch(root, args)
	if err != nil {
		if errors.Is(err, ErrHelp) {
			return 0 // help is not an error
		}
		fmt.Fprintf(errw, "error: %v\n\nRun '%s --help' for usage.\n", err, a.usagePath())
		if code == 0 {
			code = 2
		}
	}
	return code
}

// usagePath renders the tracked invocation path as a single string.
func (a *app) usagePath() string {
	return strings.Join(a.path, " ")
}

// versionCommand builds a `version` subcommand that prints "<label> <version>".
// It is attached to every component root so both the standalone binaries and
// the unified `ipcod` binary support a version query.
func versionCommand(label string) *Command {
	return &Command{
		Name:      "version",
		Short:     "Print version information",
		Long:      fmt.Sprintf("Print the %s version and exit.", label),
		flagSetup: func(fs *flag.FlagSet) {},
		Run: func(cmd *Command, args []string) error {
			fmt.Printf("%s %s\n", label, Version)
			return nil
		},
	}
}

// dispatch resolves and runs a command with the given remaining args. The
// dispatcher's only jobs are to resolve the subcommand path, surface help, and
// hand the *unmodified* remaining args to Run. Flag parsing is owned entirely
// by Run so no argument is consumed twice.
func (a *app) dispatch(c *Command, args []string) (int, error) {
	// Peel off leading subcommands.
	for {
		if len(args) == 0 {
			break
		}
		arg := args[0]
		if strings.HasPrefix(arg, "-") {
			break // a flag, not a subcommand
		}
		if arg == "help" {
			a.printHelp(c)
			return 0, ErrHelp
		}
		child := c.find(arg)
		if child == nil {
			// Not a known subcommand. If this command is runnable, treat the
			// arg as part of its args (e.g. positional). Otherwise it's a typo.
			if c.Run == nil || c.DefaultDisabled {
				return 2, fmt.Errorf("unknown command %q for %q", arg, a.usagePath())
			}
			break
		}
		c = child
		a.path = append(a.path, child.Name)
		args = args[1:]
	}

	// A lone -h/--help anywhere yields help.
	for _, arg := range args {
		if arg == "-h" || arg == "-help" || arg == "--help" {
			a.printHelp(c)
			return 0, ErrHelp
		}
	}

	// Runnable?
	if c.Run == nil {
		// No run function: this is a pure grouping command. Print help.
		a.printHelp(c)
		if len(c.Sub) > 0 {
			return 2, fmt.Errorf("%q requires a subcommand", a.usagePath())
		}
		return 0, nil
	}
	// A grouping command with subcommands but no default run -> require one.
	if c.DefaultDisabled && len(c.Sub) > 0 {
		a.printHelp(c)
		return 2, fmt.Errorf("%q requires a subcommand", a.usagePath())
	}

	if err := c.Run(c, args); err != nil {
		if errors.Is(err, ErrHelp) {
			a.printHelp(c)
			return 0, ErrHelp
		}
		return 1, err
	}
	return 0, nil
}

// find returns the child command with the given name.
func (c *Command) find(name string) *Command {
	for _, sub := range c.Sub {
		if sub.Name == name {
			return sub
		}
	}
	return nil
}

// printHelp writes the help screen for command c, using the tracked
// invocation path for the usage line.
func (a *app) printHelp(c *Command) {
	w := a.errw
	if w == nil {
		w = io.Discard
	}

	if c.Long != "" {
		fmt.Fprintf(w, "%s\n\n", strings.TrimSpace(c.Long))
	} else if c.Short != "" {
		fmt.Fprintf(w, "%s\n\n", strings.TrimSpace(c.Short))
	}

	fmt.Fprintf(w, "Usage:\n  %s", a.usagePath())
	if len(c.Sub) > 0 {
		fmt.Fprintf(w, " <command>")
	}
	fmt.Fprintf(w, " [flags]\n")

	// Flags for runnable commands.
	if c.Run != nil || c.flagSetup != nil {
		fs := flag.NewFlagSet(c.Name, flag.ContinueOnError)
		fs.SetOutput(io.Discard)
		if c.flagSetup != nil {
			c.flagSetup(fs)
		}
		renderFlagDefaults(fs, w)
	}

	// Subcommands.
	subs := visibleSubs(c)
	if len(subs) > 0 {
		fmt.Fprintf(w, "\nCommands:\n")
		width := 0
		for _, s := range subs {
			if len(s.Name) > width {
				width = len(s.Name)
			}
		}
		for _, s := range subs {
			fmt.Fprintf(w, "  %-*s  %s\n", width, s.Name, s.Short)
		}
	}

	if len(subs) > 0 {
		fmt.Fprintf(w, "\nRun '%s <command> --help' for details on a command.\n", a.usagePath())
	}
}

// visibleSubs returns non-hidden subcommands sorted by name.
func visibleSubs(c *Command) []*Command {
	var out []*Command
	for _, s := range c.Sub {
		if !s.Hidden {
			out = append(out, s)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// renderFlagDefaults prints the registered flags of fs to w in a tidy block.
func renderFlagDefaults(fs *flag.FlagSet, w io.Writer) {
	type row struct{ name, usage string }
	var rows []row
	fs.VisitAll(func(f *flag.Flag) {
		// Skip noisy default runtime flags that are already documented.
		name := f.Name
		rows = append(rows, row{"  -" + name, f.Usage})
	})
	if len(rows) == 0 {
		return
	}
	fmt.Fprintf(w, "\nFlags:\n")
	width := 0
	for _, r := range rows {
		if len(r.name) > width {
			width = len(r.name)
		}
	}
	for _, r := range rows {
		fmt.Fprintf(w, "  %-*s  %s\n", width, r.name, r.usage)
	}
}
