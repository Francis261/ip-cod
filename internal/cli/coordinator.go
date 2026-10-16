package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/ipcod/ipcod/internal/coordinator"
	"github.com/ipcod/ipcod/internal/ipdetect"
	"github.com/ipcod/ipcod/internal/store"
	"github.com/ipcod/ipcod/internal/token"
)

// Coordinator returns the root command for the coordinator binary. It is also
// reused as a subcommand of the unified `ipcod` binary.
//
// There are two ways to run it:
//
//	ipcod-coordinator run   headless: agents, admin API, IP detection
//	ipcod-coordinator web   the same, plus the dashboard UI (-p to pick a port)
//
// The laptop's public IP is discovered by the coordinator itself, by asking
// echo services outbound — no separate beacon process exists any more.
func Coordinator() *Command {
	c := &Command{
		Name:  "coordinator",
		Short: "Run the central coordinator (WebSocket server, API, IP detection, optional web UI)",
		Long: `The coordinator is the central control plane. Run it on your laptop (or
anywhere with a stable address): agents dial in over WebSocket, and the
coordinator determines this machine's public IP itself by asking echo
services outbound, then pushes changes out to every connected agent.

Two modes:
  run   headless — WebSocket, admin API, health probe (default)
  web   the same, plus the dashboard UI at /`,
		DefaultDisabled: true, // require an explicit subcommand when nested
		Sub: []*Command{
			{
				Name:  "run",
				Short: "Run the coordinator headless (default)",
				Long: `Run the coordinator without the web UI: agent WebSocket at /ws, admin API
at /api/admin/*, health probe at /healthz, and automatic public-IP
detection. Start with ` + "`web`" + ` if you want the dashboard too.`,
				flagSetup: func(fs *flag.FlagSet) { coordinatorFlags(fs, false) },
				Run:       runCoordinatorHeadless,
			},
			{
				Name:  "web",
				Short: "Run the coordinator with the web dashboard",
				Long: `Run the coordinator and serve the dashboard, e.g.

  ipcod-coordinator web -p 8443

Everything ` + "`run`" + ` does, plus the web UI at /. Bind it wherever you like and
open it in a browser.`,
				flagSetup: func(fs *flag.FlagSet) { coordinatorFlags(fs, true) },
				Run:       runCoordinatorWeb,
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
	// should run the server headless. Only require a subcommand when nested
	// under the unified binary, which sets DefaultDisabled at dispatch time.
	c.DefaultDisabled = false
	c.Run = runCoordinatorHeadless
	c.flagSetup = func(fs *flag.FlagSet) { coordinatorFlags(fs, false) }
	return c
}

// coordinatorFlags registers the flags shared by `run` and `web`. withPort
// additionally registers -p/-port, which only make sense when you are starting
// a web UI you intend to open.
func coordinatorFlags(fs *flag.FlagSet, withPort bool) {
	fs.String("listen", envOr("IPCOD_LISTEN", ":8443"), "HTTP listen address")
	fs.String("db", envOr("IPCOD_DB", "ipcod.db"), "SQLite database path")
	fs.String("admin-token", os.Getenv("IPCOD_ADMIN_TOKEN"), "admin bearer token for /api/admin (or env IPCOD_ADMIN_TOKEN)")
	fs.Bool("trust-proxy", envBool("IPCOD_TRUST_PROXY"), "honor X-Forwarded-For (set when behind a reverse proxy)")
	fs.Duration("detect-interval", envDuration("IPCOD_DETECT_INTERVAL", coordinator.DefaultDetectInterval), "how often to re-check this machine's public IP")
	fs.Duration("reconcile-interval", envDuration("IPCOD_RECONCILE_INTERVAL", coordinator.DefaultReconcileInterval), "how often to re-push the current IP to connected agents (self-heal; 0 disables)")
	fs.String("providers", envOr("IPCOD_IP_PROVIDERS", ""), "comma-separated IP echo URLs (default: built-in list)")
	fs.Bool("no-detect", envBool("IPCOD_NO_DETECT"), "disable outbound public IP detection (set the IP manually or via a beacon check-in)")
	fs.Bool("v", false, "verbose (debug) logging")
	if withPort {
		fs.Int("p", 0, "port to listen on, e.g. -p 8443 (overrides the port in -listen)")
		fs.Int("port", 0, "same as -p")
	}
}

func runCoordinatorHeadless(cmd *Command, args []string) error {
	return runCoordinatorMode(cmd, args, false)
}

func runCoordinatorWeb(cmd *Command, args []string) error {
	return runCoordinatorMode(cmd, args, true)
}

// runCoordinatorMode is the shared implementation behind `run` (dashboard=false)
// and `web` (dashboard=true).
func runCoordinatorMode(cmd *Command, args []string, dashboard bool) error {
	fs := newFlagSet(cmd.Name)
	listen := fs.String("listen", envOr("IPCOD_LISTEN", ":8443"), "HTTP listen address")
	dbPath := fs.String("db", envOr("IPCOD_DB", "ipcod.db"), "SQLite database path")
	adminToken := fs.String("admin-token", os.Getenv("IPCOD_ADMIN_TOKEN"), "admin bearer token for /api/admin")
	trustProxy := fs.Bool("trust-proxy", envBool("IPCOD_TRUST_PROXY"), "honor X-Forwarded-For (set behind a reverse proxy)")
	detectInterval := fs.Duration("detect-interval", envDuration("IPCOD_DETECT_INTERVAL", coordinator.DefaultDetectInterval), "public IP re-check interval")
	reconcileInterval := fs.Duration("reconcile-interval", envDuration("IPCOD_RECONCILE_INTERVAL", coordinator.DefaultReconcileInterval), "how often to re-push the current IP to connected agents (self-heal; 0 disables)")
	providers := fs.String("providers", envOr("IPCOD_IP_PROVIDERS", ""), "comma-separated IP echo URLs")
	noDetect := fs.Bool("no-detect", envBool("IPCOD_NO_DETECT"), "disable outbound public IP detection")
	verbose := fs.Bool("v", false, "verbose logging")
	// Registered unconditionally so one flag set serves both modes; `run`
	// simply does not advertise them in help.
	pPort := fs.Int("p", 0, "port to listen on")
	pPortLong := fs.Int("port", 0, "port to listen on")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return ErrHelp
		}
		return err
	}

	// -p/-port override the port carried by -listen.
	effectiveListen := *listen
	if *pPort > 0 {
		effectiveListen = withPort(effectiveListen, *pPort)
	} else if *pPortLong > 0 {
		effectiveListen = withPort(effectiveListen, *pPortLong)
	}

	log := newLogger(*verbose)

	if *adminToken == "" {
		msg := "admin token not set; the admin API will be disabled " +
			"until you set IPCOD_ADMIN_TOKEN or -admin-token"
		if dashboard {
			msg = "admin token not set; the admin API and dashboard will be disabled " +
				"until you set IPCOD_ADMIN_TOKEN or -admin-token"
		}
		log.Warn(msg)
	}

	st, err := store.Open(*dbPath)
	if err != nil {
		return fmt.Errorf("open store: %w", err)
	}
	defer st.Close()

	srv := coordinator.NewServer(st, log)
	cfg := coordinator.Config{
		ListenAddr: effectiveListen,
		AdminToken: *adminToken,
		TrustProxy: *trustProxy,
		Dashboard:  dashboard,
	}
	handler := coordinator.NewRouter(srv, cfg)

	// Outbound public-IP detection: the successor to the old beacon process.
	// The coordinator lives where the changing IP is and finds it itself.
	detCtx, detCancel := context.WithCancel(context.Background())
	defer detCancel()
	if *noDetect {
		log.Info("public IP detection disabled", "hint", "set the IP manually (admin API) or drop -no-detect")
	} else {
		srv.StartIPDetection(detCtx, ipdetect.Config{
			Providers: parseProviders(*providers),
			Interval:  *detectInterval,
		})
	}

	// Periodic re-assertion of the current IP to connected agents. Agents
	// verify their firewall against it and repair the difference, so pushes
	// self-heal without requiring an IP change.
	srv.StartReconcile(detCtx, *reconcileInterval)

	httpSrv := &http.Server{
		Addr:              effectiveListen,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
	}

	go func() {
		kind := "coordinator listening"
		if dashboard {
			kind = "coordinator listening (web UI on)"
		}
		log.Info(kind, "addr", effectiveListen, "db", *dbPath,
			"dashboard", dashboardURL(effectiveListen, dashboard))
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("server error", "err", err)
			os.Exit(1)
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	<-stop
	log.Info("shutting down")
	detCancel()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = httpSrv.Shutdown(ctx)
	srv.Close()
	log.Info("bye")
	return nil
}

// withPort swaps the port onto a listen address, keeping any host part:
//
//	":8443"         -> ":9000"
//	"0.0.0.0:8443"  -> "0.0.0.0:9000"
//	"[::1]:8443"    -> "[::1]:9000"
func withPort(listen string, port int) string {
	host := ""
	switch {
	case strings.HasPrefix(listen, "["): // bracketed IPv6, e.g. "[::1]:8443"
		if i := strings.LastIndex(listen, "]"); i > 0 {
			host = listen[:i+1]
		}
	default:
		if i := strings.LastIndex(listen, ":"); i > 0 {
			host = listen[:i]
		}
	}
	return fmt.Sprintf("%s:%d", host, port)
}

// dashboardURL renders a browser-friendly URL for the web UI, mapping
// wildcard binds (":8443", "0.0.0.0:8443") to loopback.
func dashboardURL(listen string, dashboard bool) string {
	if !dashboard {
		return ""
	}
	host, port, err := net.SplitHostPort(listen)
	if err != nil {
		return ""
	}
	switch host {
	case "", "0.0.0.0", "::", "[::]":
		host = "127.0.0.1"
	}
	if strings.Contains(host, ":") && !strings.HasPrefix(host, "[") {
		host = "[" + host + "]"
	}
	return fmt.Sprintf("http://%s:%s/", host, port)
}

// parseProviders turns a comma-separated provider list into a slice, dropping
// blanks. Empty input returns nil so ipdetect falls back to its defaults.
func parseProviders(s string) []string {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
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
