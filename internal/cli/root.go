package cli

import (
	"flag"
	"fmt"
)

// Version is stamped at build time via -ldflags "-X .../internal/cli.Version=...".
// It defaults to "dev" for local builds.
var Version = "dev"

// Root builds the unified `ipcod` command tree, exposing the coordinator and
// agent as subcommands alongside version/help.
//
// The same tree is what backs the individual binaries; they simply set their
// own root to one of the sub-commands (see cmd/*/main.go).
func Root() *Command {
	coord := Coordinator()
	agent := Agent()

	// Nesting under the unified binary: require an explicit subcommand instead
	// of silently defaulting to `run`, so `ipcod` alone prints help.
	coord.DefaultDisabled = true
	coord.Run = nil
	coord.flagSetup = nil
	agent.DefaultDisabled = true
	agent.Run = nil
	agent.flagSetup = nil

	return &Command{
		Name:  "ipcod",
		Short: "Dynamic IP allowlisting for your VPSes",
		Long: `ipcod keeps a set of VPS firewalls (via ufw) in sync with your
current public IP.

There are two pieces:
  coordinator   run it on your laptop — it detects your public IP itself,
                agents dial in and get pushed changes
  agent         run it on each VPS — it applies the IP via ufw`,
		DefaultDisabled: true,
		Sub: []*Command{
			coord,
			agent,
			versionCmd(),
		},
	}
}

func versionCmd() *Command {
	return &Command{
		Name:      "version",
		Short:     "Print version information",
		Long:      "Print the ipcod version and exit.",
		flagSetup: func(fs *flag.FlagSet) {},
		Run: func(cmd *Command, args []string) error {
			fmt.Printf("ipcod %s\n", Version)
			return nil
		},
	}
}
