package config

import (
	"os"
	"path/filepath"
	"testing"
)

func loadYAML(t *testing.T, projects string) (*Config, error) {
	t.Helper()
	dir := t.TempDir()
	body := "listen: 127.0.0.1:8484\ndata_dir: " + dir + "\nwebhook:\n  secret: shhh\n" + projects
	p := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return Load(p)
}

func TestLocalProjectValid(t *testing.T) {
	c, err := loadYAML(t, "projects:\n  - name: firecrew\n    path: /opt/firecrew/src\n")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	p := c.FindProjectByName("firecrew")
	if p == nil {
		t.Fatal("firecrew not found by name")
	}
	if !p.IsLocal() {
		t.Error("expected IsLocal()")
	}
	if p.Dir != "/opt/firecrew/src" {
		t.Errorf("Dir = %q, want the local path", p.Dir)
	}
	if p.Manifest != ".steward.yaml" {
		t.Errorf("Manifest default = %q", p.Manifest)
	}
}

func TestLocalProjectDerivesNameFromRepo(t *testing.T) {
	c, err := loadYAML(t, "projects:\n  - repo: alex-savin/firecrew\n    path: /opt/firecrew/src\n")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.FindProjectByName("firecrew") == nil {
		t.Error("name should default from repo for a local project")
	}
}

func TestLocalProjectValidationErrors(t *testing.T) {
	cases := map[string]string{
		"relative path":     "projects:\n  - name: x\n    path: relative/dir\n",
		"path + clone_url":  "projects:\n  - name: x\n    path: /abs\n    clone_url: https://example.com/x.git\n",
		"path without name": "projects:\n  - path: /opt/only\n",
	}
	for name, body := range cases {
		if _, err := loadYAML(t, body); err == nil {
			t.Errorf("%s: expected an error, got nil", name)
		}
	}
}

func TestGitProjectStillRequiresRepo(t *testing.T) {
	if _, err := loadYAML(t, "projects:\n  - branch: main\n"); err == nil {
		t.Error("a non-path project without a repo should error")
	}
}

func TestFindProjectByNameCaseInsensitive(t *testing.T) {
	c, err := loadYAML(t, "projects:\n  - name: FireCrew\n    path: /opt/firecrew\n")
	if err != nil {
		t.Fatal(err)
	}
	if c.FindProjectByName("firecrew") == nil {
		t.Error("FindProjectByName should be case-insensitive")
	}
	// A git-style repo match must not accidentally match a local project.
	if c.FindProject("firecrew", "main") != nil {
		t.Error("FindProject should not match a local project with no repo")
	}
}
