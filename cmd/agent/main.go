// Command agent runs on a VPS. It connects out to the coordinator over
// WebSocket and updates the local ufw firewall to allow the laptop's current
// IP to reach a configured port.
//
// It is the standalone form of `ipcod agent`.
package main

import (
	"os"

	"github.com/ipcod/ipcod/internal/cli"
)

func main() {
	root := cli.Agent()
	// Present as the standalone binary in help/usage output.
	root.Name = "ipcod-agent"
	os.Exit(cli.Execute(root, os.Args[1:], os.Stderr))
}
