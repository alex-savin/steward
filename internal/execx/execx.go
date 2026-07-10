// Package execx runs external commands with captured output, context
// cancellation, and bounded output buffers. Every external tool the agent
// drives (git, docker compose, systemctl, cp, rsync, shell hooks) goes
// through here so cancellation and error reporting behave uniformly.
package execx

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

const outputLimit = 64 * 1024

// Run executes name with args and returns combined stdout+stderr (tail-truncated).
// dir sets the working directory ("" = inherit); env entries are appended to
// the current environment. When ctx is cancelled the process is killed.
func Run(ctx context.Context, dir string, env []string, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	if len(env) > 0 {
		cmd.Env = append(os.Environ(), env...)
	}
	// If the process ignores the kill (or holds pipes open), give up waiting
	// rather than hanging the deployment forever.
	cmd.WaitDelay = 10 * time.Second

	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf

	err := cmd.Run()
	out := tail(buf.String(), outputLimit)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return out, fmt.Errorf("%s: %w (%v)", name, ctxErr, err)
		}
		return out, fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, strings.TrimSpace(out))
	}
	return out, nil
}

// RunShell executes one shell line via /bin/sh -c (backup hooks, health
// commands). The line comes from the repo's committed manifest, so anyone who
// can push to the deployed branch can run commands — same trust model as CI.
func RunShell(ctx context.Context, dir string, env []string, script string) (string, error) {
	return Run(ctx, dir, env, "/bin/sh", "-c", script)
}

func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return "…" + s[len(s)-n:]
}
