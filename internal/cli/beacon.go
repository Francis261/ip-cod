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

	"github.com/ipcod/ipcod/internal/beacon"
)

// Beacon returns the root command for the beacon binary (and the `beacon`
// subcommand of the unified binary).
func Beacon() *Command {
	beaconFlags := func(fs *flag.FlagSet) {
		fs.String("coordinator", envOr("IPCOD_COORDINATOR", ""), "coordinator http(s) beacon URL, e.g. https://coord.example.com/api/beacon")
		fs.String("token", os.Getenv("IPCOD_TOKEN"), "beacon auth token (or env IPCOD_TOKEN)")
		fs.Duration("interval", 30*time.Second, "check-in interval")
		fs.Bool("v", false, "verbose (debug) logging")
	}

	return &Command{
		Name:  "beacon",
		Short: "Run the laptop beacon (reports public IP to coordinator)",
		Long: `The beacon runs on your laptop. It periodically POSTs to the coordinator so
the coordinator can observe your current public IP (from the request's source
address) and detect when it changes.`,
		DefaultDisabled: false,
		flagSetup:       beaconFlags,
		Run:             runBeacon,
		Sub: []*Command{
			{
				Name:      "run",
				Short:     "Run the beacon daemon (default)",
				Long:      "Run the beacon, checking in on the configured interval until interrupted.",
				flagSetup: beaconFlags,
				Run:       runBeacon,
			},
			{
				Name:  "once",
				Short: "Report once and exit",
				Long:  "Perform a single check-in, print the observed IP, and exit. Handy for testing.",
				flagSetup: func(fs *flag.FlagSet) {
					fs.String("coordinator", envOr("IPCOD_COORDINATOR", ""), "coordinator http(s) beacon URL")
					fs.String("token", os.Getenv("IPCOD_TOKEN"), "beacon auth token (or env IPCOD_TOKEN)")
					fs.Bool("v", false, "verbose (debug) logging")
				},
				Run: runBeaconOnce,
			},
			versionCommand("ipcod-beacon"),
		},
	}
}

func runBeacon(cmd *Command, args []string) error {
	fs := newFlagSet(cmd.Name)
	coordURL := fs.String("coordinator", envOr("IPCOD_COORDINATOR", ""), "coordinator http(s) beacon URL")
	token := fs.String("token", os.Getenv("IPCOD_TOKEN"), "beacon auth token")
	interval := fs.Duration("interval", 30*time.Second, "check-in interval")
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
		return fmt.Errorf("beacon token required (-token or IPCOD_TOKEN)")
	}

	log := newLogger(*verbose)
	c := beacon.NewClient(beacon.Config{
		CoordinatorURL: *coordURL,
		Token:          *token,
		Interval:       *interval,
	}, log)

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	if err := c.Run(ctx); err != nil && context.Cause(ctx) == nil {
		return err
	}
	log.Info("bye")
	return nil
}

func runBeaconOnce(cmd *Command, args []string) error {
	fs := newFlagSet(cmd.Name)
	coordURL := fs.String("coordinator", envOr("IPCOD_COORDINATOR", ""), "coordinator http(s) beacon URL")
	token := fs.String("token", os.Getenv("IPCOD_TOKEN"), "beacon auth token")
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
		return fmt.Errorf("beacon token required (-token or IPCOD_TOKEN)")
	}

	log := newLogger(*verbose)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	ip, changed, err := beacon.ReportOnce(ctx, beacon.Config{
		CoordinatorURL: *coordURL,
		Token:          *token,
	}, log)
	if err != nil {
		return err
	}
	fmt.Printf("ip=%s changed=%v\n", ip, changed)
	return nil
}
