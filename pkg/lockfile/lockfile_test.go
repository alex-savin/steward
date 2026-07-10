package lockfile

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestAcquireIsExclusive(t *testing.T) {
	path := filepath.Join(t.TempDir(), "steward.lock")

	l, err := Acquire(path)
	if err != nil {
		t.Fatalf("first Acquire: %v", err)
	}

	// flock is per open-file-description, so a second Acquire in this same
	// process still contends — which is what a second agent would do.
	if _, err := Acquire(path); err == nil {
		t.Fatal("a second Acquire must fail while the lock is held")
	}

	if err := l.Release(); err != nil {
		t.Fatalf("Release: %v", err)
	}
	l2, err := Acquire(path)
	if err != nil {
		t.Fatalf("Acquire after Release: %v", err)
	}
	if err := l2.Release(); err != nil {
		t.Fatal(err)
	}
}

// Release must NOT unlink the file: two fresh instances could then lock
// different inodes at the same path and both believe they hold it.
func TestReleaseKeepsTheLockFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "steward.lock")
	l, err := Acquire(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := l.Release(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Errorf("lock file must survive Release: %v", err)
	}
}

func TestAcquireWritesPIDAndCreatesParent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "dir", "steward.lock")
	l, err := Acquire(path)
	if err != nil {
		t.Fatalf("Acquire must create parent dirs: %v", err)
	}
	defer l.Release()

	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(b)) != strconv.Itoa(os.Getpid()) {
		t.Errorf("lock file holds %q, want the holder's pid", b)
	}
}

func TestReleaseOnNilAndTwiceIsSafe(t *testing.T) {
	var l *Lock
	if err := l.Release(); err != nil {
		t.Errorf("nil Release: %v", err)
	}
	real, err := Acquire(filepath.Join(t.TempDir(), "l"))
	if err != nil {
		t.Fatal(err)
	}
	if err := real.Release(); err != nil {
		t.Fatal(err)
	}
	if err := real.Release(); err != nil {
		t.Errorf("double Release: %v", err)
	}
}
