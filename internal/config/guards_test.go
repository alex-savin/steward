package config

import (
	"strings"
	"testing"
)

// The allowlist matches the reference's repository against prefixes on a path
// boundary: tag and digest forms both pass, a lookalike sibling does not.
func TestProjectAllowsImage(t *testing.T) {
	p := Project{Images: []string{"registry.savin.nyc/familytree/app", "localhost:5000/team"}}
	digest := "@sha256:" + strings.Repeat("a", 64)
	cases := map[string]bool{
		"registry.savin.nyc/familytree/app:sha-abc":        true,
		"registry.savin.nyc/familytree/app" + digest:       true,
		"registry.savin.nyc/familytree/app:v1" + digest:    true,
		"registry.savin.nyc/familytree/app":                true,
		"registry.savin.nyc/familytree/app/worker:v1":      true,
		"localhost:5000/team/anything:v2":                  true,
		"registry.savin.nyc/familytree/app-evil:v1":        false,
		"registry.savin.nyc/familytree/app-evil" + digest:  false,
		"registry.savin.nyc/familytree/application:v1":     false,
		"registry.savin.nyc/familytree:v1":                 false,
		"evil.example/registry.savin.nyc/familytree/app:1": false,
		"familytree/app:v1":                                false, // no short-name expansion
		"localhost:5000/teamwork/app:v1":                   false,
		"localhost:5001/team/app:v1":                       false,
	}
	for ref, want := range cases {
		if got := p.AllowsImage(ref); got != want {
			t.Errorf("AllowsImage(%q) = %v, want %v", ref, got, want)
		}
	}

	// No allowlist: anything goes (the pre-allowlist behavior).
	if !(&Project{}).AllowsImage("ghcr.io/whoever/whatever:latest") {
		t.Error("an empty images list should allow any image")
	}
}

func TestImagesAllowlistLoads(t *testing.T) {
	c, err := loadYAML(t, `projects:
  - repo: alex-savin/family-tree-savin-nyc
    images:
      - registry.savin.nyc/familytree/app/
`)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	p := c.Projects[0]
	if len(p.Images) != 1 || p.Images[0] != "registry.savin.nyc/familytree/app" {
		t.Errorf("Images = %q, want the trailing slash trimmed", p.Images)
	}
	if p.AllowAnyCommit {
		t.Error("allow_any_commit must default to false")
	}
}

// An allowlist entry with a tag or digest would reject every new build, so it
// is a startup error rather than a silent deploy blocker.
func TestImagesAllowlistRejectsBadEntries(t *testing.T) {
	for _, entry := range []string{`""`, "registry.savin.nyc/familytree/app:v1", "registry.savin.nyc/familytree/app@sha256:abc", "-x"} {
		_, err := loadYAML(t, "projects:\n  - repo: o/a\n    images: ["+entry+"]\n")
		if err == nil || !strings.Contains(err.Error(), "images[0]") {
			t.Errorf("images: [%s] should be rejected, got %v", entry, err)
		}
	}
	// A registry port is not a tag.
	if _, err := loadYAML(t, "projects:\n  - repo: o/a\n    images: [\"localhost:5000/team/app\"]\n"); err != nil {
		t.Errorf("registry with port should load: %v", err)
	}
}

func TestAllowAnyCommitIsGitOnly(t *testing.T) {
	if _, err := loadYAML(t, "projects:\n  - repo: o/a\n    allow_any_commit: true\n"); err != nil {
		t.Errorf("git project with allow_any_commit should load: %v", err)
	}
	_, err := loadYAML(t, "projects:\n  - name: firecrew\n    path: /opt/firecrew/src\n    allow_any_commit: true\n")
	if err == nil || !strings.Contains(err.Error(), "allow_any_commit") {
		t.Errorf("local project with allow_any_commit should fail, got %v", err)
	}
}
