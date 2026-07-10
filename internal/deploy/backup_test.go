package deploy

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/alex-savin/steward/internal/config"
)

// These tests cover the two places the agent can destroy data it was built to
// protect: rotateBackups (os.RemoveAll) and restoreVolumes (rsync --delete over
// a live directory). A regression here is unrecoverable on a homelab with no
// second backup, so they are exercised against real files, real cp, real rsync.

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func mkdirs(t *testing.T, base string, names ...string) {
	t.Helper()
	for _, n := range names {
		if err := os.MkdirAll(filepath.Join(base, n), 0o755); err != nil {
			t.Fatal(err)
		}
	}
}

func entries(t *testing.T, dir string) []string {
	t.Helper()
	es, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range es {
		out = append(out, e.Name())
	}
	slices.Sort(out)
	return out
}

func TestSnapshotDirPattern(t *testing.T) {
	valid := []string{"20260710T013655Z-9f2a1c3", "20260101T000000Z-abc1234567a", "20991231T235959Z-a"}
	for _, v := range valid {
		if !snapshotDirPattern.MatchString(v) {
			t.Errorf("%q should match the snapshot pattern", v)
		}
	}
	// Anything the agent did not create must be untouchable by rotation.
	invalid := []string{
		"important-data", "postgres", "", ".", "..",
		"20260710T013655Z", "20260710T013655Z-", "20260710T013655Z-ZZZZ",
		"x20260710T013655Z-9f2a1c3", "20260710T013655Z-9f2a1c3-extra",
		"2026-07-10T01:36:55Z-9f2a1c3",
	}
	for _, v := range invalid {
		if snapshotDirPattern.MatchString(v) {
			t.Errorf("%q must NOT match the snapshot pattern", v)
		}
	}
}

func TestRotateBackupsKeepsNewestAndSparesStrangers(t *testing.T) {
	dest := t.TempDir()
	// Three snapshots (lexicographic == chronological), plus files and dirs the
	// agent never created and must never delete.
	mkdirs(t, dest,
		"20260101T000000Z-aaaaaaa",
		"20260202T000000Z-bbbbbbb",
		"20260303T000000Z-ccccccc",
		"manual-backup-do-not-delete",
		"postgres",
	)
	if err := os.WriteFile(filepath.Join(dest, "notes.txt"), []byte("keep me"), 0o644); err != nil {
		t.Fatal(err)
	}

	m := &config.Manifest{
		Backup: config.Backup{
			Retention: config.Retention{KeepLast: 2},
			Volumes:   []config.VolumeBackup{{Source: "/unused", Destination: dest}},
		},
	}
	rotateBackups(quiet(), m)

	got := entries(t, dest)
	want := []string{
		"20260202T000000Z-bbbbbbb", // newest two kept
		"20260303T000000Z-ccccccc",
		"manual-backup-do-not-delete",
		"notes.txt",
		"postgres",
	}
	if !slices.Equal(got, want) {
		t.Errorf("after rotation:\n got %v\nwant %v", got, want)
	}
}

func TestRotateBackupsKeepLastOneAndDedupesDestinations(t *testing.T) {
	dest := t.TempDir()
	mkdirs(t, dest, "20260101T000000Z-aaaaaaa", "20260202T000000Z-bbbbbbb")

	// The same destination listed twice must not rotate twice.
	m := &config.Manifest{Backup: config.Backup{
		Retention: config.Retention{KeepLast: 1},
		Volumes: []config.VolumeBackup{
			{Source: "/a", Destination: dest},
			{Source: "/b", Destination: dest},
		},
	}}
	rotateBackups(quiet(), m)

	if got := entries(t, dest); !slices.Equal(got, []string{"20260202T000000Z-bbbbbbb"}) {
		t.Errorf("got %v, want only the newest snapshot", got)
	}
}

// keep_last < 1 must be a no-op, never "delete everything".
func TestRotateBackupsRefusesToDeleteEverything(t *testing.T) {
	dest := t.TempDir()
	mkdirs(t, dest, "20260101T000000Z-aaaaaaa", "20260202T000000Z-bbbbbbb")
	m := &config.Manifest{Backup: config.Backup{
		Retention: config.Retention{KeepLast: 0},
		Volumes:   []config.VolumeBackup{{Source: "/a", Destination: dest}},
	}}
	rotateBackups(quiet(), m)
	if got := entries(t, dest); len(got) != 2 {
		t.Errorf("keep_last=0 must not delete anything, got %v", got)
	}
}

// The rollback path: the live directory must end up byte-identical to the
// snapshot, including removal of files created after the backup was taken.
func TestRestoreVolumesDirectoryIsExact(t *testing.T) {
	root := t.TempDir()
	live := filepath.Join(root, "live")
	snapDir := filepath.Join(root, "snap", "app_data")
	mkdirs(t, live, "")
	mkdirs(t, snapDir, "sub")

	if err := os.WriteFile(filepath.Join(snapDir, "keep.txt"), []byte("good"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(snapDir, "sub", "nested.txt"), []byte("nested"), 0o644); err != nil {
		t.Fatal(err)
	}
	// The live dir has drifted: a changed file and a file the backup lacks.
	if err := os.WriteFile(filepath.Join(live, "keep.txt"), []byte("CORRUPTED"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(live, "written-by-bad-deploy.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	snap := &Snapshot{Volumes: []VolumeSnapshot{{Source: live, Path: snapDir}}}
	if err := restoreVolumes(context.Background(), quiet(), snap); err != nil {
		t.Fatalf("restoreVolumes: %v", err)
	}

	b, err := os.ReadFile(filepath.Join(live, "keep.txt"))
	if err != nil || string(b) != "good" {
		t.Errorf("keep.txt = %q, %v; want restored content", b, err)
	}
	if b, err := os.ReadFile(filepath.Join(live, "sub", "nested.txt")); err != nil || string(b) != "nested" {
		t.Errorf("nested file not restored: %q %v", b, err)
	}
	if _, err := os.Stat(filepath.Join(live, "written-by-bad-deploy.txt")); !os.IsNotExist(err) {
		t.Error("restore must delete files absent from the snapshot (rsync --delete semantics)")
	}
	// The restore must land INSIDE the live dir, never as a nested copy of it.
	if _, err := os.Stat(filepath.Join(live, filepath.Base(snapDir))); err == nil {
		t.Error("restore nested the snapshot dir inside the target (trailing-slash bug)")
	}
}

// backup.volumes[].source may be a single file (e.g. a SQLite database).
func TestRestoreVolumesSingleFile(t *testing.T) {
	root := t.TempDir()
	live := filepath.Join(root, "app.db")
	snapFile := filepath.Join(root, "app.db.snap")

	if err := os.WriteFile(snapFile, []byte("original"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(live, []byte("corrupted"), 0o600); err != nil {
		t.Fatal(err)
	}

	snap := &Snapshot{Volumes: []VolumeSnapshot{{Source: live, Path: snapFile}}}
	if err := restoreVolumes(context.Background(), quiet(), snap); err != nil {
		t.Fatalf("restoreVolumes: %v", err)
	}
	if b, _ := os.ReadFile(live); string(b) != "original" {
		t.Errorf("file restore = %q, want original", b)
	}
}

// A snapshot that vanished must be reported, never silently skipped — and it
// must not wipe the live directory on the way out.
func TestRestoreVolumesMissingSnapshotErrorsWithoutDeleting(t *testing.T) {
	root := t.TempDir()
	live := filepath.Join(root, "live")
	mkdirs(t, live, "")
	if err := os.WriteFile(filepath.Join(live, "data.txt"), []byte("precious"), 0o644); err != nil {
		t.Fatal(err)
	}

	snap := &Snapshot{Volumes: []VolumeSnapshot{{Source: live, Path: filepath.Join(root, "gone")}}}
	if err := restoreVolumes(context.Background(), quiet(), snap); err == nil {
		t.Fatal("expected an error for a missing snapshot")
	}
	if b, _ := os.ReadFile(filepath.Join(live, "data.txt")); string(b) != "precious" {
		t.Error("a missing snapshot must not destroy live data")
	}
}

func TestRestoreVolumesNilSnapshotIsNoOp(t *testing.T) {
	if err := restoreVolumes(context.Background(), quiet(), nil); err != nil {
		t.Errorf("nil snapshot: %v", err)
	}
}

// Backup commands run BEFORE the volume copy, so a dump written into a
// backed-up directory lands in this run's snapshot (spec Step 2 ordering).
func TestRunBackupCommandsRunBeforeVolumeCopy(t *testing.T) {
	root := t.TempDir()
	src := filepath.Join(root, "data")
	dest := filepath.Join(root, "backups")
	mkdirs(t, src, "")

	m := &config.Manifest{Backup: config.Backup{
		Retention: config.Retention{KeepLast: 3},
		Commands:  []string{"echo dumped > " + filepath.Join(src, "dump.sql")},
		Volumes:   []config.VolumeBackup{{Source: src, Destination: dest}},
	}}

	snap, err := runBackup(context.Background(), quiet(), root, m, "9f2a1c3abc", nil)
	if err != nil {
		t.Fatalf("runBackup: %v", err)
	}
	if len(snap.Volumes) != 1 {
		t.Fatalf("expected one volume snapshot, got %+v", snap.Volumes)
	}
	if _, err := os.Stat(filepath.Join(snap.Volumes[0].Path, "dump.sql")); err != nil {
		t.Error("the backup command's dump is missing from this run's snapshot")
	}
	if !snapshotDirPattern.MatchString(filepath.Base(filepath.Dir(snap.Volumes[0].Path))) {
		t.Errorf("snapshot dir %q does not match the rotation pattern — it could never be rotated", snap.Volumes[0].Path)
	}
}

// A failing backup command must abort before anything is copied or restarted.
func TestRunBackupFailingCommandAborts(t *testing.T) {
	root := t.TempDir()
	dest := filepath.Join(root, "backups")
	m := &config.Manifest{Backup: config.Backup{
		Retention: config.Retention{KeepLast: 3},
		Commands:  []string{"exit 3"},
		Volumes:   []config.VolumeBackup{{Source: root, Destination: dest}},
	}}
	if _, err := runBackup(context.Background(), quiet(), root, m, "abc1234", nil); err == nil {
		t.Fatal("expected a failing backup command to abort the deployment")
	}
	if _, err := os.Stat(dest); !os.IsNotExist(err) {
		t.Error("no snapshot should exist after a failed backup command")
	}
}

// A missing source is a warning, not a failure: the first deploy has no volume.
func TestRunBackupSkipsMissingSource(t *testing.T) {
	root := t.TempDir()
	m := &config.Manifest{Backup: config.Backup{
		Retention: config.Retention{KeepLast: 3},
		Volumes:   []config.VolumeBackup{{Source: filepath.Join(root, "nope"), Destination: filepath.Join(root, "b")}},
	}}
	snap, err := runBackup(context.Background(), quiet(), root, m, "abc1234", nil)
	if err != nil {
		t.Fatalf("runBackup: %v", err)
	}
	if len(snap.Volumes) != 0 {
		t.Errorf("missing source should yield no snapshot, got %+v", snap.Volumes)
	}
}
