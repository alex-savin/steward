package deploy

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"

	"github.com/alex-savin/steward/internal/config"
	"github.com/alex-savin/steward/internal/execx"
)

var composeFileCandidates = []string{"compose.yaml", "compose.yml", "docker-compose.yml", "docker-compose.yaml"}

// composeStack drives `docker compose` (or docker-compose / podman-compose,
// whichever exists) from the checkout directory, so relative paths and the
// decrypted .env resolve exactly as they would by hand.
type composeStack struct {
	workdir string
	file    string
	argv    []string
	env     []string
	deps    []config.Dependency
	log     *slog.Logger
}

func newComposeStack(m *config.Manifest, workdir string, env []string, log *slog.Logger) (*composeStack, error) {
	file := m.Compose.File
	if file == "" {
		for _, c := range composeFileCandidates {
			if _, err := os.Stat(filepath.Join(workdir, c)); err == nil {
				file = c
				break
			}
		}
	}
	if file == "" {
		return nil, fmt.Errorf("no compose file in %s (looked for %v; set compose.file to override)", workdir, composeFileCandidates)
	}
	argv, err := composeCommand()
	if err != nil {
		return nil, err
	}
	return &composeStack{workdir: workdir, file: file, argv: argv, env: env, deps: m.Dependencies, log: log}, nil
}

// composeCommand picks the first available compose implementation.
func composeCommand() ([]string, error) {
	if _, err := exec.LookPath("docker"); err == nil {
		return []string{"docker", "compose"}, nil
	}
	if _, err := exec.LookPath("docker-compose"); err == nil {
		return []string{"docker-compose"}, nil
	}
	if _, err := exec.LookPath("podman-compose"); err == nil {
		return []string{"podman-compose"}, nil
	}
	return nil, fmt.Errorf("no compose implementation found (tried docker, docker-compose, podman-compose)")
}

func (s *composeStack) Name() string { return config.StackDockerCompose }

func (s *composeStack) Deploy(ctx context.Context) error {
	// Bring explicitly-ordered dependencies (e.g. the database) up first so
	// application containers don't crash-loop against a cold datastore.
	for _, dep := range s.deps {
		if !dep.StartFirst {
			continue
		}
		s.log.Info("starting dependency first", "service", dep.Name)
		if _, err := s.compose(ctx, "up", "-d", dep.Name); err != nil {
			return fmt.Errorf("start dependency %s: %w", dep.Name, err)
		}
	}

	s.log.Info("bringing compose stack up", "file", s.file)
	if _, err := s.compose(ctx, "up", "-d", "--remove-orphans"); err != nil {
		return err
	}
	return nil
}

// Stop halts the project's containers (used before restoring volume backups
// during rollback) without removing networks or volumes.
func (s *composeStack) Stop(ctx context.Context) error {
	_, err := s.compose(ctx, "stop")
	return err
}

// Logs returns the tail of every service's output.
func (s *composeStack) Logs(ctx context.Context, tailLines int) (string, error) {
	return s.compose(ctx, "logs", "--no-color", "--tail", strconv.Itoa(tailLines))
}

func (s *composeStack) compose(ctx context.Context, args ...string) (string, error) {
	full := make([]string, 0, len(s.argv)+2+len(args))
	full = append(full, s.argv[1:]...)
	full = append(full, "-f", s.file)
	full = append(full, args...)
	return execx.Run(ctx, s.workdir, s.env, s.argv[0], full...)
}
