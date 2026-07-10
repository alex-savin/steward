package config

import (
	"path/filepath"
	"strings"
	"testing"
)

// The example manifests are documentation: they are quoted in docs/ and copied
// into real repos. Load them through the real parser so a schema change can
// never leave them silently invalid.
func TestExampleManifestsAreValid(t *testing.T) {
	examples := []string{
		filepath.Join("..", "..", "configs", "steward.example.yaml"),
		filepath.Join("..", "..", "examples", "my-awesome-app", ".steward.yaml"),
	}
	for _, path := range examples {
		t.Run(filepath.Base(filepath.Dir(path))+"/"+filepath.Base(path), func(t *testing.T) {
			m, err := LoadManifest(path)
			if err != nil {
				t.Fatalf("LoadManifest(%s): %v", path, err)
			}
			if m.Project == "" || m.Stack == "" {
				t.Fatalf("%s: incomplete manifest: %+v", path, m)
			}
		})
	}
}

// The example app's backup command is a multi-line YAML plain scalar. Confirm
// it folds into the single shell line the docs claim, with the redirect intact.
func TestExampleBackupCommandFolds(t *testing.T) {
	m, err := LoadManifest(filepath.Join("..", "..", "examples", "my-awesome-app", ".steward.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Backup.Commands) != 1 {
		t.Fatalf("expected 1 backup command, got %d", len(m.Backup.Commands))
	}
	cmd := m.Backup.Commands[0]
	if strings.Contains(cmd, "\n") {
		t.Errorf("command should fold to one line, got %q", cmd)
	}
	for _, want := range []string{"pg_dump", "> /var/lib/homelab/app_data/", "${STEWARD_BACKUP_STAMP}"} {
		if !strings.Contains(cmd, want) {
			t.Errorf("command %q missing %q", cmd, want)
		}
	}
}
