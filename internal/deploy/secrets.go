package deploy

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/alex-savin/steward/internal/config"
)

// decryptSecrets runs Step 1 of the lifecycle: for every declared file, run
// `sops --decrypt` (age identity supplied via SOPS_AGE_KEY_FILE) and write
// the plaintext inside the checkout with owner-only permissions. Source and
// destination were validated at manifest load to stay inside the repo.
func decryptSecrets(ctx context.Context, log *slog.Logger, workdir string, m *config.Manifest) error {
	if m.Secrets.Provider == "" || len(m.Secrets.Files) == 0 {
		log.Debug("no secrets configured; skipping decryption")
		return nil
	}
	if m.Secrets.Provider != "sops" {
		return fmt.Errorf("unsupported secrets provider %q (only \"sops\")", m.Secrets.Provider)
	}
	if _, err := exec.LookPath("sops"); err != nil {
		return fmt.Errorf("sops binary not found in PATH: %w", err)
	}

	for _, f := range m.Secrets.Files {
		src := filepath.Join(workdir, f.Source)
		dst := filepath.Join(workdir, f.Destination)

		// Capture stdout (the plaintext) separately from stderr so decrypted
		// material never ends up in an error message or log line.
		cmd := exec.CommandContext(ctx, "sops", "--decrypt", src)
		cmd.Dir = workdir
		cmd.Env = append(os.Environ(), "SOPS_AGE_KEY_FILE="+m.Secrets.KeyFile)
		// Matches execx.Run: a process that ignores the context's kill (or a
		// grandchild holding these pipes open) must not hang the deployment.
		cmd.WaitDelay = 10 * time.Second
		var out, errb bytes.Buffer
		cmd.Stdout = &out
		cmd.Stderr = &errb
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("sops --decrypt %s: %w: %s", f.Source, err, strings.TrimSpace(errb.String()))
		}

		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(dst, out.Bytes(), 0o600); err != nil {
			return fmt.Errorf("write decrypted %s: %w", f.Destination, err)
		}
		log.Info("secret decrypted", "source", f.Source, "destination", f.Destination)
	}
	return nil
}
