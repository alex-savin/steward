// Package state persists the small amount of per-project memory Steward
// needs between runs — most importantly the last known-good commit, which is
// the rollback target when a deployment fails its health check.
package state

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"time"
)

// Store keeps one JSON file per project under dir.
type Store struct {
	dir string
}

func New(dir string) *Store { return &Store{dir: dir} }

// Project is everything remembered about a project between deployments.
type Project struct {
	LastGoodSHA   string    `json:"last_good_sha"`
	LastGoodImage string    `json:"last_good_image,omitempty"` // image-centric rollback target
	DeployedAt    time.Time `json:"deployed_at"`
}

// Load returns the stored state, or the zero value if none exists yet.
func (s *Store) Load(name string) (Project, error) {
	var p Project
	b, err := os.ReadFile(s.path(name))
	if errors.Is(err, fs.ErrNotExist) {
		return p, nil
	}
	if err != nil {
		return p, err
	}
	if err := json.Unmarshal(b, &p); err != nil {
		return Project{}, err
	}
	return p, nil
}

// Save writes atomically (temp file + rename) so a crash mid-write can never
// corrupt the rollback target.
func (s *Store) Save(name string, p Project) error {
	if err := os.MkdirAll(s.dir, 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.path(name) + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, s.path(name))
}

func (s *Store) path(name string) string { return filepath.Join(s.dir, name+".json") }
