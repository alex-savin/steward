package deploy

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/alex-savin/steward/internal/config"
	"github.com/alex-savin/steward/internal/execx"
)

// verifyHealth executes Step 4 of the lifecycle: the manifest's health check,
// retried with linear backoff. The per-attempt timeout comes from the
// manifest; the overall deployment deadline still applies through ctx.
func verifyHealth(ctx context.Context, log *slog.Logger, workdir string, hc config.HealthCheck, env []string) error {
	if hc.Type == "" {
		log.Info("no health check configured; treating deploy as successful")
		return nil
	}
	retries := hc.Retries
	if retries < 1 {
		retries = 1
	}

	var lastErr error
	for attempt := 1; attempt <= retries; attempt++ {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("health check aborted: %w", err)
		}
		lastErr = healthAttempt(ctx, workdir, hc, env)
		if lastErr == nil {
			log.Info("health check passed", "attempt", attempt)
			return nil
		}
		log.Warn("health check attempt failed", "attempt", attempt, "of", retries, "error", lastErr)
		if attempt < retries {
			select {
			case <-ctx.Done():
				return fmt.Errorf("health check aborted: %w", ctx.Err())
			case <-time.After(time.Duration(attempt) * 2 * time.Second):
			}
		}
	}
	return fmt.Errorf("health check failed after %d attempts: %w", retries, lastErr)
}

func healthAttempt(ctx context.Context, workdir string, hc config.HealthCheck, env []string) error {
	attemptCtx, cancel := context.WithTimeout(ctx, hc.Timeout.Std())
	defer cancel()

	switch hc.Type {
	case config.HealthHTTP:
		req, err := http.NewRequestWithContext(attemptCtx, http.MethodGet, hc.Endpoint, nil)
		if err != nil {
			return err
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		if resp.StatusCode < 200 || resp.StatusCode > 299 {
			return fmt.Errorf("unexpected status %s from %s", resp.Status, hc.Endpoint)
		}
		return nil
	case config.HealthCommand:
		_, err := execx.RunShell(attemptCtx, workdir, env, hc.Command)
		return err
	default:
		return fmt.Errorf("unsupported health check type %q", hc.Type)
	}
}
