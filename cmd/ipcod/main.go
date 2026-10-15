// Command ipcod is the unified binary exposing the coordinator, agent, and
// beacon as subcommands:
//
//	ipcod coordinator ...
//	ipcod agent ...
//	ipcod beacon ...
//	ipcod version
//
// It shares all logic with the standalone binaries (ipcod-coordinator,
// ipcod-agent, ipcod-beacon); this is just a single-file convenience bundle.
package main

import (
	"os"

	"github.com/ipcod/ipcod/internal/cli"
)

func main() {
	os.Exit(cli.Execute(cli.Root(), os.Args[1:], os.Stderr))
}
