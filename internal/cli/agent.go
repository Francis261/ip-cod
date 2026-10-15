package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/ipcod/ipcod/internal/agent"
)

// Agent returns the root command for the agent binary (and the `agent`
// subcommand of the unified binary).
func Agent() *Command {
	agentFlags := func(fs *flag.FlagSet) {
		fs.String("coordinator", envOr("IPCOD_COORDINATOR", ""), "coordinator ws:// or wss:// URL, e.g. wss://coord.example.com/ws")
		fs.String("token", os.Getenv("IPCOD_TOKEN"), "agent auth token (or env IPCOD_TOKEN)")
		fs.String("name", envOr("IPCOD_NAME", defaultHostname()), "display name for this agent")
		fs.String("ufw", envOr("IPCOD_UFW", "ufw"), "path to the ufw binary")
		fs.Bool("dry-run", envBool("IPCOD_DRY_RUN"), "log ufw commands instead of running them")
		fs.Bool("v", false, "verbose (debug) logging")
	}

	return &Command{
		Name:  "agent",
		Short: "Run the VPS agent (connects to coordinator, manages ufw)",
		Long: `The agent runs on a VPS. It dials the coordinator over WebSocket (outbound
only, so no inbound port is needed) and updates the local ufw firewall to
allow the laptop's current public IP to reach a configured port.`,
		// Standalone default: run the daemon.
		DefaultDisabled: false,
		flagSetup:       agentFlags,
		Run:             runAgent,
		Sub: []*Command{
			{
				Name:      "run",
				Short:     "Run the agent daemon (default)",
				Long:      "Run the agent daemon, maintaining the coordinator connection and applying firewall updates.",
				flagSetup: agentFlags,
				Run:       runAgent,
			},
			{
				Name:  "check",
				Short: "Verify connectivity and auth, then exit",
				Long: `Dial the coordinator, authenticate with the configured token, and report
whether the connection succeeded. Exits 0 on success, non-zero on failure.
Useful for validating a deployment without starting the daemon.`,
				flagSetup: func(fs *flag.FlagSet) {
					fs.String("coordinator", envOr("IPCOD_COORDINATOR", ""), "coordinator ws:// or wss:// URL")
					fs.String("token", os.Getenv("IPCOD_TOKEN"), "agent auth token (or env IPCOD_TOKEN)")
					fs.String("name", envOr("IPCOD_NAME", defaultHostname()), "display name for this agent")
					fs.Duration("timeout", 15*time.Second, "how long to wait before giving up")
					fs.Bool("v", false, "verbose (debug) logging")
				},
				Run: runAgentCheck,
			},
			versionCommand("ipcod-agent"),
		},
	}
}

func runAgent(cmd *Command, args []string) error {
	fs := newFlagSet(cmd.Name)
	coordURL := fs.String("coordinator", envOr("IPCOD_COORDINATOR", ""), "coordinator ws:// or wss:// URL")
	token := fs.String("token", os.Getenv("IPCOD_TOKEN"), "agent auth token")
	name := fs.String("name", envOr("IPCOD_NAME", defaultHostname()), "display name")
	ufwBin := fs.String("ufw", envOr("IPCOD_UFW", "ufw"), "path to the ufw binary")
	dryRun := fs.Bool("dry-run", envBool("IPCOD_DRY_RUN"), "log ufw commands instead of running them")
	verbose := fs.Bool("v", false, "verbose logging")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return ErrHelp
		}
		return err
	}

	log := newLogger(*verbose)

	if *coordURL == "" {
		return fmt.Errorf("coordinator URL required (-coordinator or IPCOD_COORDINATOR)")
	}
	if *token == "" {
		return fmt.Errorf("agent token required (-token or IPCOD_TOKEN)")
	}

	cfg := agent.Config{
		CoordinatorURL: *coordURL,
		Token:          *token,
		Name:           *name,
		UFWBin:         *ufwBin,
		DryRun:         *dryRun,
		MinBackoff:     2 * time.Second,
		MaxBackoff:     2 * time.Minute,
	}

	c := agent.NewClient(cfg, log)
	if *dryRun {
		log.Info("running in dry-run mode; no firewall changes will be made")
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	if err := c.Run(ctx); err != nil && context.Cause(ctx) == nil {
		return err
	}
	log.Info("bye")
	return nil
}

func runAgentCheck(cmd *Command, args []string) error {
	fs := newFlagSet(cmd.Name)
	coordURL := fs.String("coordinator", envOr("IPCOD_COORDINATOR", ""), "coordinator ws:// or wss:// URL")
	token := fs.String("token", os.Getenv("IPCOD_TOKEN"), "agent auth token")
	name := fs.String("name", envOr("IPCOD_NAME", defaultHostname()), "display name")
	timeout := fs.Duration("timeout", 15*time.Second, "how long to wait before giving up")
	verbose := fs.Bool("v", false, "verbose logging")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return ErrHelp
		}
		return err
	}

	if *coordURL == "" {
		return fmt.Errorf("coordinator URL required (-coordinator or IPCOD_COORDINATOR)")
	}
	if *token == "" {
		return fmt.Errorf("agent token required (-token or IPCOD_TOKEN)")
	}

	log := newLogger(*verbose)
	c := agent.NewClient(agent.Config{
		CoordinatorURL: *coordURL,
		Token:          *token,
		Name:           *name,
	}, log)

	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()

	if err := c.Check(ctx); err != nil {
		return fmt.Errorf("check failed: %w", err)
	}
	fmt.Println("ok: authenticated with coordinator")
	return nil
}

func defaultHostname() string {
	h, err := os.Hostname()
	if err != nil || h == "" {
		return "agent"
	}
	return h
}
