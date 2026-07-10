package deploy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/alex-savin/steward/internal/config"
	"github.com/alex-savin/steward/internal/execx"
)

// quadletExtensions are the unit kinds Podman's systemd generator understands.
var quadletExtensions = []string{".container", ".pod", ".volume", ".network", ".kube", ".image", ".build"}

// quadletStack installs the repo's quadlet unit files into the rootless
// location (~/.config/containers/systemd/), reloads the user systemd
// instance, and restarts the generated services. Each deploy CONVERGES the
// installed set: units this project installed previously but that no longer
// exist in the checkout are stopped and removed, so renames, deletions, and
// rollbacks away from a bad commit don't leave orphaned services running.
type quadletStack struct {
	project    string
	workdir    string
	sourceDir  string
	installDir string
	env        []string
	deps       []config.Dependency
	log        *slog.Logger
}

func newQuadletStack(m *config.Manifest, workdir string, env []string, log *slog.Logger) (*quadletStack, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("resolve home dir for quadlet install: %w", err)
	}
	return &quadletStack{
		project:    m.Project,
		workdir:    workdir,
		sourceDir:  filepath.Join(workdir, m.Quadlet.Dir),
		installDir: filepath.Join(home, ".config", "containers", "systemd"),
		env:        env,
		deps:       m.Dependencies,
		log:        log,
	}, nil
}

func (s *quadletStack) Name() string { return config.StackQuadlet }

// ownershipFile records which unit files this project installed, so the next
// deploy (or a rollback) can stop and remove units that disappeared from the
// repo. Its name is not a quadlet unit kind, so the generator ignores it.
func (s *quadletStack) ownershipFile() string {
	return filepath.Join(s.installDir, "."+s.project+".steward-units.json")
}

// installedUnits returns the unit file names recorded by the previous deploy
// of this project (nil when none or unreadable).
func (s *quadletStack) installedUnits() []string {
	b, err := os.ReadFile(s.ownershipFile())
	if err != nil {
		return nil
	}
	var units []string
	if err := json.Unmarshal(b, &units); err != nil {
		return nil
	}
	return units
}

func (s *quadletStack) Deploy(ctx context.Context) error {
	files, err := s.unitFiles()
	if err != nil {
		return err
	}
	if len(files) == 0 {
		return fmt.Errorf("no quadlet unit files (*.container, *.pod, ...) in %s", s.sourceDir)
	}
	current := basenames(files)

	if err := os.MkdirAll(s.installDir, 0o755); err != nil {
		return err
	}

	// Converge: stop and remove units a previous deploy of this project
	// installed that are gone from the checkout.
	for _, stale := range s.installedUnits() {
		if slices.Contains(current, stale) {
			continue
		}
		if svc := serviceFor(stale); svc != "" {
			s.log.Info("stopping stale service", "unit", svc)
			if _, err := execx.Run(ctx, "", s.env, "systemctl", "--user", "stop", svc); err != nil {
				s.log.Warn("could not stop stale service", "unit", svc, "error", err)
			}
		}
		s.log.Info("removing stale quadlet", "file", stale)
		if err := os.Remove(filepath.Join(s.installDir, stale)); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("remove stale quadlet %s: %w", stale, err)
		}
	}

	for _, f := range files {
		data, err := os.ReadFile(f)
		if err != nil {
			return err
		}
		target := filepath.Join(s.installDir, filepath.Base(f))
		if err := os.WriteFile(target, data, 0o644); err != nil {
			return fmt.Errorf("install quadlet %s: %w", filepath.Base(f), err)
		}
	}
	if err := s.recordUnits(current); err != nil {
		s.log.Warn("could not record installed unit set", "error", err)
	}
	s.log.Info("quadlets installed", "count", len(files), "dir", s.installDir)

	if _, err := execx.Run(ctx, "", s.env, "systemctl", "--user", "daemon-reload"); err != nil {
		return err
	}
	for _, svc := range s.services(current) {
		s.log.Info("restarting service", "unit", svc)
		if _, err := execx.Run(ctx, "", s.env, "systemctl", "--user", "restart", svc); err != nil {
			return fmt.Errorf("restart %s: %w", svc, err)
		}
	}
	return nil
}

// Stop halts every service this project's units generate (used before
// restoring volume backups during rollback). Best effort across all units.
func (s *quadletStack) Stop(ctx context.Context) error {
	units := s.installedUnits()
	if units == nil {
		files, err := s.unitFiles()
		if err != nil {
			return err
		}
		units = basenames(files)
	}
	var errs []error
	for _, u := range units {
		svc := serviceFor(u)
		if svc == "" {
			continue
		}
		if _, err := execx.Run(ctx, "", s.env, "systemctl", "--user", "stop", svc); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// Logs returns the tail of each generated service's journal.
func (s *quadletStack) Logs(ctx context.Context, tailLines int) (string, error) {
	units := s.installedUnits()
	if units == nil {
		files, err := s.unitFiles()
		if err != nil {
			return "", err
		}
		units = basenames(files)
	}
	var b strings.Builder
	var errs []error
	for _, u := range units {
		svc := serviceFor(u)
		if svc == "" {
			continue
		}
		out, err := execx.Run(ctx, "", s.env,
			"journalctl", "--user", "--no-pager", "-u", svc, "-n", strconv.Itoa(tailLines))
		if err != nil {
			errs = append(errs, err)
			continue
		}
		b.WriteString("--- " + svc + " ---\n" + out + "\n")
	}
	return b.String(), errors.Join(errs...)
}

func (s *quadletStack) recordUnits(units []string) error {
	b, err := json.Marshal(units)
	if err != nil {
		return err
	}
	return os.WriteFile(s.ownershipFile(), b, 0o644)
}

// unitFiles lists the quadlet files in the repo's quadlet dir (sorted for
// deterministic install order).
func (s *quadletStack) unitFiles() ([]string, error) {
	entries, err := os.ReadDir(s.sourceDir)
	if err != nil {
		return nil, fmt.Errorf("quadlet dir: %w", err)
	}
	var files []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if slices.Contains(quadletExtensions, filepath.Ext(e.Name())) {
			files = append(files, filepath.Join(s.sourceDir, e.Name()))
		}
	}
	slices.Sort(files)
	return files, nil
}

// services derives the systemd services to restart from unit file names,
// dependencies marked start_first ahead of the rest.
func (s *quadletStack) services(unitNames []string) []string {
	var first, rest []string
	for _, u := range unitNames {
		svc := serviceFor(u)
		if svc == "" {
			continue
		}
		name := strings.TrimSuffix(u, filepath.Ext(u))
		if s.isStartFirst(name) {
			first = append(first, svc)
		} else {
			rest = append(rest, svc)
		}
	}
	return append(first, rest...)
}

// serviceFor maps a quadlet file name to the service podman-systemd generates
// for it: foo.container -> foo.service, bar.pod -> bar-pod.service,
// baz.kube -> baz.service. Volume/network/image/build units don't run as
// long-lived services — systemd activates them as dependencies.
func serviceFor(unitFile string) string {
	ext := filepath.Ext(unitFile)
	name := strings.TrimSuffix(unitFile, ext)
	switch ext {
	case ".container", ".kube":
		return name + ".service"
	case ".pod":
		return name + "-pod.service"
	default:
		return ""
	}
}

func (s *quadletStack) isStartFirst(unitName string) bool {
	for _, d := range s.deps {
		if d.StartFirst && d.Name == unitName {
			return true
		}
	}
	return false
}

func basenames(paths []string) []string {
	out := make([]string, 0, len(paths))
	for _, p := range paths {
		out = append(out, filepath.Base(p))
	}
	return out
}
