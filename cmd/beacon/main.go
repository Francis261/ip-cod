// Command beacon runs on the user's laptop. It periodically reports to the
// coordinator so the coordinator can observe the laptop's current public IP
// and detect when it changes.
//
// It is the standalone form of `ipcod beacon`.
package main

import (
	"os"

	"github.com/ipcod/ipcod/internal/cli"
)

func main() {
	root := cli.Beacon()
	// Present as the standalone binary in help/usage output.
	root.Name = "ipcod-beacon"
	os.Exit(cli.Execute(root, os.Args[1:], os.Stderr))
}
