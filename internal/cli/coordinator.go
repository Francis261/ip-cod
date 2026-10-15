package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/ipcod/ipcod/internal/coordinator"
	"github.com/ipcod/ipcod/internal/store"
	"github.com/ipcod/ipcod/internal/token"
)

// Coordinator returns the root command for the coordinator binary. It is also
// reused as a subcommand of the unified `ipcod` binary.
func Coordinator() *Command {
	c := &Command{
		Name:  "coordinator",
		Short: "Run the central coordinator (WebSocket server, API, dashboard)",
		Long: `The coordinator is the central control plane. It accepts outbound WebSocket
connections from agents, receives periodic check-ins from beacons to learn
your laptop's public IP, and pushes IP changes back out to every connected
agent. It also serves an admin API and web dashboard.`,
		DefaultDisabled: true, // require an explicit subcommand when nested
		Sub: []*Command{
			{
				Name:  "run",
				Short: "Run the coordinator server (default)",
				Long:  "Run the coordinator server. This is the default when no subcommand is given.",
				flagSetup: func(fs *flag.FlagSet) {
					fs.String("listen", envOr("IPCOD_LISTEN", ":8443"), "HTTP listen address")
					fs.String("db", envOr("IPCOD_DB", "ipcod.db"), "SQLite database path")
					fs.String("admin-token", os.Getenv("IPCOD_ADMIN_TOKEN"), "admin bearer token for /api/admin (or env IPCOD_ADMIN_TOKEN)")
					fs.Bool("trust-proxy", envBool("IPCOD_TRUST_PROXY"), "honor X-Forwarded-For (set when behind a reverse proxy)")
					fs.Bool("v", false, "verbose (debug) logging")
				},
				Run: runCoordinator,
			},
			{
				Name:  "gen-admin-token",
				Short: "Generate a random admin bearer token",
				Long: `Generate a cryptographically random admin bearer token and print it to
stdout. Use it for IPCOD_ADMIN_TOKEN or -admin-token.`, flagSetup: func(fs *flag.FlagSet) {},
				Run: runGenAdminToken,
			},
			versionCommand("ipcod-coordinator"),
		},
	}
	// When invoked standalone (not nested), `coordinator` with no subcommand
	// should run the server. Only require a subcommand when nested under the
	// unified binary, which sets DefaultDisabled at dispatch time. Here we keep
	// DefaultDisabled=false so `ipcod-coordinator` with no args just runs.
	c.DefaultDisabled = false
	c.Run = runCoordinator
	c.flagSetup = coordinatorRunFlags()
	return c
}

func coordinatorRunFlags() func(fs *flag.FlagSet) {
	return func(fs *flag.FlagSet) {
		fs.String("listen", envOr("IPCOD_LISTEN", ":8443"), "HTTP listen address")
		fs.String("db", envOr("IPCOD_DB", "ipcod.db"), "SQLite database path")
		fs.String("admin-token", os.Getenv("IPCOD_ADMIN_TOKEN"), "admin bearer token for /api/admin (or env IPCOD_ADMIN_TOKEN)")
		fs.Bool("trust-proxy", envBool("IPCOD_TRUST_PROXY"), "honor X-Forwarded-For (set when behind a reverse proxy)")
		fs.Bool("v", false, "verbose (debug) logging")
	}
}

func runCoordinator(cmd *Command, args []string) error {
	fs := newFlagSet(cmd.Name)
	listen := fs.String("listen", envOr("IPCOD_LISTEN", ":8443"), "HTTP listen address")
	dbPath := fs.String("db", envOr("IPCOD_DB", "ipcod.db"), "SQLite database path")
	adminToken := fs.String("admin-token", os.Getenv("IPCOD_ADMIN_TOKEN"), "admin bearer token for /api/admin")
	trustProxy := fs.Bool("trust-proxy", envBool("IPCOD_TRUST_PROXY"), "honor X-Forwarded-For (set behind a reverse proxy)")
	verbose := fs.Bool("v", false, "verbose logging")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return ErrHelp
		}
		return err
	}

	log := newLogger(*verbose)

	if *adminToken == "" {
		log.Warn("admin token not set; the admin API and dashboard will be disabled " +
			"until you set IPCOD_ADMIN_TOKEN or -admin-token")
	}

	st, err := store.Open(*dbPath)
	if err != nil {
		return fmt.Errorf("open store: %w", err)
	}
	defer st.Close()

	srv := coordinator.NewServer(st, log)
	cfg := coordinator.Config{
		ListenAddr: *listen,
		AdminToken: *adminToken,
		TrustProxy: *trustProxy,
	}
	handler := coordinator.NewRouter(srv, cfg)

	httpSrv := &http.Server{
		Addr:              *listen,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
	}

	go func() {
		log.Info("coordinator listening", "addr", *listen, "db", *dbPath)
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("server error", "err", err)
			os.Exit(1)
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	<-stop
	log.Info("shutting down")

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = httpSrv.Shutdown(ctx)
	srv.Close()
	log.Info("bye")
	return nil
}

func runGenAdminToken(cmd *Command, args []string) error {
	tok, err := token.GenerateAdmin()
	if err != nil {
		return err
	}
	fmt.Println(tok)
	return nil
}

// newFlagSet builds a flag set that discards default error output (the CLI
// framework renders help itself).
func newFlagSet(name string) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(io_Discard{})
	return fs
}

// io_Discard is a tiny io.Writer sink to keep flag from writing to stderr.
type io_Discard struct{}

func (io_Discard) Write(p []byte) (int, error) { return len(p), nil }

func newLogger(verbose bool) *slog.Logger {
	level := slog.LevelInfo
	if verbose {
		level = slog.LevelDebug
	}
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))
}
