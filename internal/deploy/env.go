package deploy

import (
	"github.com/alex-savin/steward/internal/config"
)

// jobEnv is the deployment identity exported to every child process the
// lifecycle spawns: compose, quadlet's systemctl calls, backup commands, and
// command health checks.
//
// It exists so a compose file can pin the image to the exact commit being
// deployed — `image: ghcr.io/owner/app:sha-${STEWARD_SHA}` — without CI
// having to rewrite files in the checkout. Compose substitutes variables from
// the process environment, which it inherits from us.
//
// During rollback the same variables describe the commit being restored, not
// the one that failed, so the previous version's compose file resolves to the
// image it was originally deployed with.
func jobEnv(m *config.Manifest, project, ref, sha string) []string {
	env := []string{
		"STEWARD_PROJECT=" + project,
		"STEWARD_SHA=" + sha,
		"STEWARD_SHORT_SHA=" + shortSHA(sha),
		"STEWARD_REF=" + ref,
	}
	if m != nil {
		env = append(env,
			"STEWARD_STACK="+m.Stack,
			"STEWARD_ENVIRONMENT="+m.Environment,
		)
	}
	return env
}
