// Package deploy implements the deployment lifecycle engine:
//
//	decrypt -> backup -> deploy -> verify -> (rollback) -> notify
//
// The Orchestrator walks that sequence; Stack abstracts the two supported
// container runtimes (Docker Compose and Podman Quadlets).
package deploy

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/alex-savin/steward/internal/config"
)

// Stack abstracts how a project's containers are (re)started.
type Stack interface {
	// Name identifies the driver in logs ("docker-compose" or "quadlet").
	Name() string
	// Deploy brings the stack to the state described by the current checkout,
	// starting dependencies marked start_first before everything else.
	Deploy(ctx context.Context) error
	// Stop halts the stack's services. Rollback calls this before restoring
	// volume snapshots so containers aren't writing into the data being
	// restored underneath them.
	Stop(ctx context.Context) error
	// Logs returns the tail of the stack's container/service output. It runs
	// after a failed deploy or health check and before rollback, which would
	// otherwise destroy the only evidence of why the app didn't come up.
	// Best effort: an error here must never mask the deployment's own error.
	Logs(ctx context.Context, tailLines int) (string, error)
}

// NewStack picks the driver declared by the manifest's `stack:` key. env is
// the deployment identity (see jobEnv) exported to the driver's subprocesses.
func NewStack(m *config.Manifest, workdir string, env []string, log *slog.Logger) (Stack, error) {
	switch m.Stack {
	case config.StackDockerCompose:
		return newComposeStack(m, workdir, env, log)
	case config.StackQuadlet:
		return newQuadletStack(m, workdir, env, log)
	default:
		return nil, fmt.Errorf("unsupported stack %q", m.Stack)
	}
}
