package deploy

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/alex-savin/steward/internal/config"
)

// pinImage is a no-op for compose stacks and for an empty image.
func TestPinImageNoOp(t *testing.T) {
	compose := &config.Manifest{Stack: config.StackDockerCompose, Image: config.ImageConfig{LocalTag: "x"}}
	if err := pinImage(context.Background(), quiet(), compose, "ghcr.io/o/a:1", nil); err != nil {
		t.Errorf("compose stack should be a no-op: %v", err)
	}
	quad := &config.Manifest{Stack: config.StackQuadlet}
	if err := pinImage(context.Background(), quiet(), quad, "", nil); err != nil {
		t.Errorf("empty image should be a no-op: %v", err)
	}
}

// For a quadlet stack, pinImage pulls the delivered image and re-tags it to the
// manifest's LocalTag. Verified with a fake `podman` on PATH.
func TestPinImageQuadletPullsAndTags(t *testing.T) {
	dir := t.TempDir()
	logf := filepath.Join(dir, "podman.log")
	shim := "#!/bin/sh\necho \"$@\" >> " + logf + "\nexit 0\n"
	if err := os.WriteFile(filepath.Join(dir, "podman"), []byte(shim), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	m := &config.Manifest{Stack: config.StackQuadlet, Image: config.ImageConfig{LocalTag: "localhost/go-nttd:live"}}
	if err := pinImage(context.Background(), quiet(), m, "ghcr.io/o/go-nttd:sha-abc", nil); err != nil {
		t.Fatalf("pinImage: %v", err)
	}
	got, _ := os.ReadFile(logf)
	log := string(got)
	if !strings.Contains(log, "pull ghcr.io/o/go-nttd:sha-abc") {
		t.Errorf("expected a pull, got: %q", log)
	}
	if !strings.Contains(log, "tag ghcr.io/o/go-nttd:sha-abc localhost/go-nttd:live") {
		t.Errorf("expected a re-tag, got: %q", log)
	}
}

// The delivered image lands in podman's argv; an option-shaped ref is refused.
func TestPinImageRejectsOptionLikeRef(t *testing.T) {
	m := &config.Manifest{Stack: config.StackQuadlet}
	if err := pinImage(context.Background(), quiet(), m, "--authfile=/etc/shadow", nil); err == nil {
		t.Error("an image starting with '-' must be refused")
	}
}

// repinImage (rollback) re-tags a previous image to the LocalTag.
func TestRepinImage(t *testing.T) {
	dir := t.TempDir()
	logf := filepath.Join(dir, "podman.log")
	if err := os.WriteFile(filepath.Join(dir, "podman"), []byte("#!/bin/sh\necho \"$@\" >> "+logf+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	m := &config.Manifest{Stack: config.StackQuadlet, Image: config.ImageConfig{LocalTag: "localhost/go-nttd:live"}}
	if err := repinImage(context.Background(), quiet(), m, "ghcr.io/o/go-nttd:sha-prev", nil); err != nil {
		t.Fatalf("repinImage: %v", err)
	}
	got, _ := os.ReadFile(logf)
	if !strings.Contains(string(got), "tag ghcr.io/o/go-nttd:sha-prev localhost/go-nttd:live") {
		t.Errorf("expected re-tag to previous image, got: %q", got)
	}
	// A repin with no LocalTag is a no-op (no podman call).
	m2 := &config.Manifest{Stack: config.StackQuadlet}
	if err := repinImage(context.Background(), quiet(), m2, "x:1", nil); err != nil {
		t.Errorf("no LocalTag should be a no-op: %v", err)
	}
}

// STEWARD_IMAGE is exported to child processes for compose ${STEWARD_IMAGE}.
func TestJobEnvIncludesImage(t *testing.T) {
	env := jobEnv(&config.Manifest{Stack: config.StackQuadlet}, "firecrew", "", "", "ghcr.io/o/go-nttd:sha-abc")
	var found bool
	for _, e := range env {
		if e == "STEWARD_IMAGE=ghcr.io/o/go-nttd:sha-abc" {
			found = true
		}
	}
	if !found {
		t.Errorf("STEWARD_IMAGE missing from env: %v", env)
	}
}
