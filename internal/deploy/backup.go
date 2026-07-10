package deploy

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"time"

	"github.com/alex-savin/steward/internal/config"
	"github.com/alex-savin/steward/internal/execx"
)

// Snapshot records where this run's pre-deploy backups landed so a rollback
// can restore them.
type Snapshot struct {
	Stamp   string
	Volumes []VolumeSnapshot
}

// VolumeSnapshot is one backed-up volume.
type VolumeSnapshot struct {
	Source string // live path that was backed up
	Path   string // copy inside the snapshot dir
}

// snapshotDirPattern matches only dirs created by runBackup, so rotation can
// never delete anything else living in the destination directory.
var snapshotDirPattern = regexp.MustCompile(`^\d{8}T\d{6}Z-[0-9a-f]{1,12}$`)

// runBackup executes Step 2 of the lifecycle: run the manifest's backup
// commands (e.g. pg_dump), copy each declared volume into a timestamped
// snapshot dir, then apply the retention policy (keep_last).
//
// Commands run BEFORE the volume copies, so a dump written into a directory
// listed under backup.volumes is captured by this run's snapshot rather than
// the next one. Commands see the deployment env plus STEWARD_BACKUP_STAMP.
func runBackup(ctx context.Context, log *slog.Logger, workdir string, m *config.Manifest, sha string, env []string) (*Snapshot, error) {
	snap := &Snapshot{Stamp: time.Now().UTC().Format("20060102T150405Z") + "-" + shortSHA(sha)}

	cmdEnv := append(append([]string(nil), env...), "STEWARD_BACKUP_STAMP="+snap.Stamp)
	for _, cmdLine := range m.Backup.Commands {
		log.Info("running backup command", "cmd", cmdLine)
		if _, err := execx.RunShell(ctx, workdir, cmdEnv, cmdLine); err != nil {
			return nil, fmt.Errorf("backup command failed: %w", err)
		}
	}

	for _, v := range m.Backup.Volumes {
		if _, err := os.Stat(v.Source); errors.Is(err, fs.ErrNotExist) {
			log.Warn("backup volume missing; skipping (first deploy?)", "source", v.Source)
			continue
		}
		dir := filepath.Join(v.Destination, snap.Stamp)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, err
		}
		log.Info("backing up volume", "source", v.Source, "snapshot", dir)
		// cp -a preserves permissions/ownership/symlinks on both linux and darwin.
		if _, err := execx.Run(ctx, "", nil, "cp", "-a", v.Source, dir+string(os.PathSeparator)); err != nil {
			return nil, fmt.Errorf("backup %s: %w", v.Source, err)
		}
		snap.Volumes = append(snap.Volumes, VolumeSnapshot{
			Source: v.Source,
			Path:   filepath.Join(dir, filepath.Base(v.Source)),
		})
	}

	rotateBackups(log, m)
	return snap, nil
}

// rotateBackups enforces backup.retention.keep_last per destination dir.
// Rotation failures are logged but never fail the deployment.
func rotateBackups(log *slog.Logger, m *config.Manifest) {
	keep := m.Backup.Retention.KeepLast
	if keep < 1 {
		return
	}
	seen := map[string]bool{}
	for _, v := range m.Backup.Volumes {
		if seen[v.Destination] {
			continue
		}
		seen[v.Destination] = true

		entries, err := os.ReadDir(v.Destination)
		if err != nil {
			continue
		}
		var snaps []string
		for _, e := range entries {
			if e.IsDir() && snapshotDirPattern.MatchString(e.Name()) {
				snaps = append(snaps, e.Name())
			}
		}
		slices.Sort(snaps) // timestamp prefix: lexicographic == chronological
		for _, old := range snaps[:max(0, len(snaps)-keep)] {
			path := filepath.Join(v.Destination, old)
			log.Info("rotating out old backup", "path", path)
			if err := os.RemoveAll(path); err != nil {
				log.Warn("could not remove old backup", "path", path, "error", err)
			}
		}
	}
}

// restoreVolumes copies the snapshot contents back over the live paths during
// rollback. rsync --delete gives an exact restore when available (it ships
// with both macOS and most linux distros); cp -a overlays otherwise.
func restoreVolumes(ctx context.Context, log *slog.Logger, snap *Snapshot) error {
	if snap == nil {
		return nil
	}
	var errs []error
	for _, v := range snap.Volumes {
		log.Info("restoring volume from backup", "snapshot", v.Path, "target", v.Source)
		info, err := os.Stat(v.Path)
		if err != nil {
			errs = append(errs, fmt.Errorf("restore %s: %w", v.Source, err))
			continue
		}
		switch {
		case !info.IsDir():
			// A file-typed backup source (e.g. a single SQLite database).
			_, err = execx.Run(ctx, "", nil, "cp", "-a", v.Path, v.Source)
		default:
			if _, lookErr := exec.LookPath("rsync"); lookErr == nil {
				_, err = execx.Run(ctx, "", nil, "rsync", "-a", "--delete", v.Path+"/", v.Source+"/")
			} else {
				_, err = execx.Run(ctx, "", nil, "cp", "-a", v.Path+"/.", v.Source)
			}
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("restore %s: %w", v.Source, err))
		}
	}
	return errors.Join(errs...)
}

func shortSHA(sha string) string {
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}
