package deploy

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/alex-savin/steward/internal/config"
	"github.com/alex-savin/steward/internal/execx"
)

// pinImage makes `image` the one the stack will run.
//
// For a quadlet stack it pulls the image and re-tags the manifest's stable
// LocalTag to it — quadlet `Image=` lines are static, so re-tagging is how a
// delivered image reaches the running unit on restart. For compose it is a
// no-op: the compose file references ${STEWARD_IMAGE} (jobEnv exports it) and
// `compose up` pulls it.
//
// The delivered `image` should be an immutable ref (a per-build tag like
// `sha-abc` or a `repo@sha256:…` digest) so the value later recorded as the
// rollback target points at exactly this build. Digest refs need no special
// casing: `podman pull` and `podman tag` both accept them as-is.
func pinImage(ctx context.Context, log *slog.Logger, m *config.Manifest, image string, env []string) error {
	if image == "" || m.Stack != config.StackQuadlet {
		return nil
	}
	// The image comes from the webhook payload and lands in podman's argv.
	if strings.HasPrefix(image, "-") {
		return fmt.Errorf("invalid image reference %q", image)
	}
	log.Info("pulling delivered image", "image", image)
	if _, err := execx.Run(ctx, "", env, "podman", "pull", image); err != nil {
		return fmt.Errorf("pull %s: %w", image, err)
	}
	if tag := m.Image.LocalTag; tag != "" {
		if _, err := execx.Run(ctx, "", env, "podman", "tag", image, tag); err != nil {
			return fmt.Errorf("tag %s -> %s: %w", image, tag, err)
		}
		log.Info("pinned image", "delivered", image, "local_tag", tag)
	}
	return nil
}

// repinImage re-points the manifest's LocalTag to a previously-deployed image
// during rollback (the image must still be in the local store). No-op for
// non-quadlet stacks or when there is no LocalTag.
func repinImage(ctx context.Context, log *slog.Logger, m *config.Manifest, image string, env []string) error {
	if image == "" || m.Stack != config.StackQuadlet || m.Image.LocalTag == "" {
		return nil
	}
	if _, err := execx.Run(ctx, "", env, "podman", "tag", image, m.Image.LocalTag); err != nil {
		return fmt.Errorf("re-pin %s -> %s: %w", image, m.Image.LocalTag, err)
	}
	log.Warn("rolled back image", "to", image, "local_tag", m.Image.LocalTag)
	return nil
}
