package state

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// The rollback target. A wrong value here redeploys the wrong commit.
func TestSaveLoadRoundTrip(t *testing.T) {
	s := New(filepath.Join(t.TempDir(), "state"))
	want := Project{LastGoodSHA: "9f2a1c3abc", DeployedAt: time.Now().UTC().Truncate(time.Second)}

	if err := s.Save("app", want); err != nil {
		t.Fatalf("Save: %v", err)
	}
	got, err := s.Load("app")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.LastGoodSHA != want.LastGoodSHA || !got.DeployedAt.Equal(want.DeployedAt) {
		t.Errorf("round trip = %+v, want %+v", got, want)
	}
}

// An unknown project is the zero value, not an error: a first deploy has no
// state, and rollback must read that as "nothing to roll back to".
func TestLoadMissingIsZeroValue(t *testing.T) {
	s := New(filepath.Join(t.TempDir(), "state"))
	got, err := s.Load("never-deployed")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.LastGoodSHA != "" {
		t.Errorf("got %+v, want zero value", got)
	}
}

func TestSaveOverwrites(t *testing.T) {
	s := New(filepath.Join(t.TempDir(), "state"))
	if err := s.Save("app", Project{LastGoodSHA: "old"}); err != nil {
		t.Fatal(err)
	}
	if err := s.Save("app", Project{LastGoodSHA: "new"}); err != nil {
		t.Fatal(err)
	}
	got, _ := s.Load("app")
	if got.LastGoodSHA != "new" {
		t.Errorf("LastGoodSHA = %q, want new", got.LastGoodSHA)
	}
}

// Corrupt state must surface as an error rather than a silent zero value that
// would report "nothing to roll back to" while a good SHA sat on disk.
func TestLoadCorruptFileErrors(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "app.json"), []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := New(dir).Load("app"); err == nil {
		t.Fatal("expected an error for corrupt state")
	}
}

// Save is atomic (temp file + rename); no .tmp litter may survive.
func TestSaveLeavesNoTempFile(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state")
	s := New(dir)
	if err := s.Save("app", Project{LastGoodSHA: "abc"}); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if filepath.Ext(e.Name()) == ".tmp" {
			t.Errorf("temp file survived Save: %s", e.Name())
		}
	}
}
