// Command steward is the Steward daemon: a local deployment agent
// that receives GitHub webhooks (behind Nginx), and deploys Docker Compose or
// Podman Quadlet stacks with SOPS secrets, pre-deploy backups, health-checked
// rollout, and automatic rollback.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/alex-savin/steward/internal/config"
	"github.com/alex-savin/steward/internal/deploy"
	"github.com/alex-savin/steward/internal/dispatch"
	"github.com/alex-savin/steward/internal/server"
	"github.com/alex-savin/steward/pkg/lockfile"
)

// Injected by the Makefile via -ldflags.
var (
	version   = "dev"
	commit    = "none"
	buildDate = "unknown"
)

func main() {
	// `steward validate [path...]` lints manifests offline. It exists so
	// an application repo can check its .steward.yaml in CI, next to `go test`,
	// instead of discovering a typo by failing a real deployment on the host.
	if len(os.Args) > 1 && os.Args[1] == "validate" {
		if err := validate(os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, "steward:", err)
			os.Exit(1)
		}
		return
	}
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "steward:", err)
		os.Exit(1)
	}
}

// validate loads each manifest through the exact parser the agent uses, so
// unknown keys, bad durations, and escaping secret paths all fail here too.
func validate(paths []string) error {
	if len(paths) == 0 {
		paths = []string{".steward.yaml"}
	}
	var bad int
	for _, p := range paths {
		m, err := config.LoadManifest(p)
		if err != nil {
			bad++
			fmt.Fprintf(os.Stderr, "✗ %s\n%v\n", p, err)
			continue
		}
		fmt.Printf("✓ %s — project %q, stack %q, mode %q, timeout %s\n",
			p, m.Project, m.Stack, m.Deployment.Mode, m.Deployment.Timeout.Std())
	}
	if bad > 0 {
		return fmt.Errorf("%d of %d manifest(s) invalid", bad, len(paths))
	}
	return nil
}

func run() error {
	var (
		configPath  = flag.String("config", "/etc/steward/config.yaml", "path to the agent configuration file")
		showVersion = flag.Bool("version", false, "print version and exit")
		debug       = flag.Bool("debug", false, "enable debug logging")
	)
	flag.Parse()

	if *showVersion {
		fmt.Printf("steward %s (%s, built %s)\n", version, commit, buildDate)
		return nil
	}

	level := slog.LevelInfo
	if *debug {
		level = slog.LevelDebug
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))
	slog.SetDefault(log)

	cfg, err := config.Load(*configPath)
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	// One agent per host: refuse to start if another instance holds the lock.
	lock, err := lockfile.Acquire(cfg.LockFile)
	if err != nil {
		return err
	}
	defer lock.Release()

	warnOpenPolicies(log, cfg)

	orch := deploy.New(cfg, log)
	disp := dispatch.New(dispatch.Options{
		Run:         orch.Run,
		Log:         log,
		DefaultMode: cfg.Defaults.DeployMode,
		ResolveMode: func(p config.Project) string {
			m, err := config.LoadManifest(filepath.Join(p.Dir, p.Manifest))
			if err != nil {
				return "" // no checkout yet (first deploy) — use the default
			}
			return m.Deployment.Mode
		},
		OnSuperseded: orch.ReportSuperseded,
	})
	srv := server.New(cfg, log, disp)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	errc := make(chan error, 1)
	go func() { errc <- srv.ListenAndServe() }()

	log.Info("steward started", "version", version, "projects", len(cfg.Projects))

	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}

	log.Info("shutting down", "grace", cfg.ShutdownGrace.Std())

	// Stop taking webhooks first, then let in-flight deployments drain.
	httpCtx, cancelHTTP := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancelHTTP()
	if err := srv.Shutdown(httpCtx); err != nil {
		log.Warn("http shutdown", "error", err)
	}

	drainCtx, cancelDrain := context.WithTimeout(context.Background(), cfg.ShutdownGrace.Std())
	defer cancelDrain()
	disp.Shutdown(drainCtx)

	log.Info("shutdown complete")
	return nil
}

// warnOpenPolicies logs, once at startup, each project that runs with one of
// the deploy guards relaxed, so a permissive config is visible in the journal.
func warnOpenPolicies(log *slog.Logger, cfg *config.Config) {
	for _, p := range cfg.Projects {
		if len(p.Images) == 0 {
			log.Warn("project accepts any deploy_image; set images: to restrict it", "project", p.Name)
		}
		if p.AllowAnyCommit {
			log.Warn("project deploys any commit on the origin, not only its branch (allow_any_commit)",
				"project", p.Name, "branch", p.Branch)
		}
	}
}
