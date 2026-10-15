// Command coordinator runs the ipcod coordinator: a WebSocket server that
// agents connect to, an HTTP endpoint for beacons to report the laptop IP, and
// an admin API + dashboard.
//
// It is the standalone form of `ipcod coordinator`.
package main

import (
	"os"

	"github.com/ipcod/ipcod/internal/cli"
)

func main() {
	root := cli.Coordinator()
	// Present as the standalone binary in help/usage output.
	root.Name = "ipcod-coordinator"
	os.Exit(cli.Execute(root, os.Args[1:], os.Stderr))
}
