package btrfs

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"gobackupper/internal/backup"
	"gobackupper/internal/process"
)

// The controlled CLI below exercises adapter decisions in TempDir; it is not
// evidence of behavior on a mounted Btrfs filesystem.
func fixture(t *testing.T) (*Repository, backup.Job, string) {
	t.Helper()
	root := t.TempDir()
	target, snaps, dest := filepath.Join(root, "target"), filepath.Join(root, "snaps"), filepath.Join(root, "target", "workspace")
	for _, p := range []string{target, snaps, dest} {
		if err := os.Mkdir(p, 0700); err != nil {
			t.Fatal(err)
		}
	}
	bin := filepath.Join(root, "btrfs")
	script := `#!/bin/sh
set -eu
case "$1:${2:-}" in
version:*) echo 'btrfs-progs v6.0';;
subvolume:snapshot) test "${FAIL_SNAPSHOT:-}" != 1 || exit 8; mkdir "$5"; : > "$5/.subvolume"; echo ro=true > "$5/.ro";;
subvolume:create) mkdir "$3"; : > "$3/.subvolume"; echo ro=false > "$3/.ro";;
property:get) if test -f "$3/.property-denied"; then echo 'ERROR: Operation not permitted' >&2; exit 1; fi; if test ! -f "$3/.subvolume" || test -f "$3/.invalid"; then echo 'ERROR: object is not compatible with property: ro' >&2; exit 1; fi; cat "$3/.ro";;
property:set) test "${FAIL_COMPRESSION:-}" != 1 || exit 9; echo "$5" > "$3/.compression";;
*) echo unexpected command "$@" >&2; exit 22;;
esac
`
	if err := os.WriteFile(bin, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dest, ".subvolume"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dest, ".ro"), []byte("ro=false\n"), 0600); err != nil {
		t.Fatal(err)
	}
	job := backup.Job{Name: "job", Destination: dest, SnapshotPrefix: "workspace", Compression: "zstd"}
	r := New(backup.Plan{TargetRoot: target, SnapshotRoot: snaps, Jobs: []backup.Job{job}}, process.Runner{})
	r.Binary = bin
	r.filesystemIDAtFunc = func(string) (string, error) { return "fixture-fsid", nil }
	return r, job, root
}

func TestRotationFirstRepeatedAndGapped(t *testing.T) {
	r, job, _ := fixture(t)
	ctx := context.Background()
	date := time.Date(2026, 11, 3, 0, 0, 0, 0, time.Local)
	ws := backup.Workspace{Job: job}
	first, err := r.Snapshot(ctx, ws, date)
	if err != nil || first.PreviousPath != "" {
		t.Fatalf("first=%+v err=%v", first, err)
	}
	if _, err = r.Snapshot(ctx, ws, date); err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat(first.Path + ".0"); err != nil {
		t.Fatal(err)
	}
	if err = os.Mkdir(first.Path+".2", 0700); err != nil {
		t.Fatal(err)
	}
	third, err := r.Snapshot(ctx, ws, date)
	if err != nil || third.PreviousPath != first.Path+".1" {
		t.Fatalf("third=%+v err=%v", third, err)
	}
	if _, err = os.Stat(first.Path + ".2"); err != nil {
		t.Fatalf("gapped suffix overwritten: %v", err)
	}
}

func TestSnapshotFailureReturnsPreviousLocation(t *testing.T) {
	r, job, _ := fixture(t)
	ctx := context.Background()
	date := time.Date(2026, 11, 3, 0, 0, 0, 0, time.Local)
	ws := backup.Workspace{Job: job}
	old, err := r.Snapshot(ctx, ws, date)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("FAIL_SNAPSHOT", "1")
	got, err := r.Snapshot(ctx, ws, date)
	if err == nil || got.PreviousPath != old.Path+".0" || !strings.Contains(err.Error(), got.PreviousPath) {
		t.Fatalf("snapshot=%+v err=%v", got, err)
	}
	if _, e := os.Stat(got.PreviousPath); e != nil {
		t.Fatalf("previous snapshot lost: %v", e)
	}
	if _, e := os.Stat(got.Path); !os.IsNotExist(e) {
		t.Fatalf("failed daily path exists: %v", e)
	}
}

func TestPrepareCompressionFailureAndExistingUnchanged(t *testing.T) {
	r, job, root := fixture(t)
	ctx := context.Background()
	job.Destination = filepath.Join(root, "target", "new")
	t.Setenv("FAIL_COMPRESSION", "1")
	if _, err := r.Prepare(ctx, job); err == nil {
		t.Fatal("expected compression failure")
	}
	if _, err := os.Stat(job.Destination); err != nil {
		t.Fatalf("created subvolume should remain: %v", err)
	}
	if err := os.WriteFile(filepath.Join(job.Destination, ".ro"), []byte("ro=false\n"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("FAIL_COMPRESSION", "")
	if _, err := r.Prepare(ctx, job); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(job.Destination, ".compression")); !os.IsNotExist(err) {
		t.Fatalf("existing compression changed: %v", err)
	}
}

func TestPrepareRejectsOrdinaryAndSymlinkLeaves(t *testing.T) {
	r, job, root := fixture(t)
	ordinary := filepath.Join(root, "ordinary")
	if err := os.Mkdir(ordinary, 0700); err != nil {
		t.Fatal(err)
	}
	job.Destination = ordinary
	if _, err := r.Prepare(context.Background(), job); err == nil {
		t.Fatal("ordinary directory accepted")
	}
	link := filepath.Join(root, "link")
	if err := os.Symlink(ordinary, link); err != nil {
		t.Fatal(err)
	}
	job.Destination = link
	if _, err := r.Prepare(context.Background(), job); err == nil {
		t.Fatal("symlink leaf accepted")
	}
}

func TestAcquireLockContentionReleaseAndPartialFailure(t *testing.T) {
	r, _, _ := fixture(t)
	ctx := context.Background()
	release, err := r.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = r.Acquire(ctx); err == nil {
		t.Fatal("contended lock acquired")
	}
	if err = release(); err != nil {
		t.Fatal(err)
	}
	snapLock := filepath.Join(r.plan.SnapshotRoot, ".gobackupper.lock")
	fd, err := unix.Open(snapLock, unix.O_CREAT|unix.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(fd)
	if err = unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB); err != nil {
		t.Fatal(err)
	}
	if _, err = r.Acquire(ctx); err == nil {
		t.Fatal("second lock contention ignored")
	}
	targetFD, err := unix.Open(filepath.Join(r.plan.TargetRoot, ".gobackupper.lock"), unix.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err = unix.Flock(targetFD, unix.LOCK_EX|unix.LOCK_NB); err != nil {
		t.Fatalf("target lock leaked after partial acquisition: %v", err)
	}
	_ = unix.Flock(targetFD, unix.LOCK_UN)
	_ = unix.Close(targetFD)
	_ = unix.Flock(fd, unix.LOCK_UN)
	release, err = r.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	_ = release()
}

func TestCheckValidatesExistingWorkspaceMetadata(t *testing.T) {
	r, job, _ := fixture(t)
	if err := r.Check(context.Background()); err != nil {
		t.Fatalf("valid workspace: %v", err)
	}
	if err := os.WriteFile(filepath.Join(job.Destination, ".ro"), []byte("ro=true\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := r.Check(context.Background()); err == nil {
		t.Fatal("read-only workspace accepted")
	}
	ordinary := filepath.Join(filepath.Dir(r.plan.TargetRoot), "ordinary")
	if err := os.Mkdir(ordinary, 0700); err != nil {
		t.Fatal(err)
	}
	r.plan.Jobs[0].Destination = ordinary
	if err := r.Check(context.Background()); err == nil {
		t.Fatal("ordinary destination accepted by Check")
	}
}

func TestCheckAndPrepareUseROPropertyToVerifyWorkspace(t *testing.T) {
	r, job, _ := fixture(t)
	if err := r.Check(context.Background()); err != nil {
		t.Fatalf("check with readable ro property: %v", err)
	}
	workspace, err := r.Prepare(context.Background(), job)
	if err != nil || workspace.Job.Name != job.Name {
		t.Fatalf("workspace=%+v err=%v", workspace, err)
	}
}

func TestValidateWarnsWhenROPropertyPermissionIsDenied(t *testing.T) {
	r, job, _ := fixture(t)
	if err := os.WriteFile(filepath.Join(job.Destination, ".property-denied"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	warnings, err := r.Validate(context.Background())
	if err != nil || len(warnings) != 1 || !strings.Contains(warnings[0], "writable state") {
		t.Fatalf("warnings=%q err=%v", warnings, err)
	}
	if err := r.Check(context.Background()); err == nil {
		t.Fatal("strict check accepted inaccessible ro property")
	}
}

func TestValidateKeepsNonPermissionPropertyErrorsFatal(t *testing.T) {
	r, job, _ := fixture(t)
	if err := os.WriteFile(filepath.Join(job.Destination, ".invalid"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	warnings, err := r.Validate(context.Background())
	if err == nil || len(warnings) != 0 {
		t.Fatalf("warnings=%q err=%v", warnings, err)
	}
}
func TestFilesystemIDFromStatfs(t *testing.T) {
	stat := unix.Statfs_t{Type: unix.BTRFS_SUPER_MAGIC}
	stat.Fsid.Val = [2]int32{0x1234, 0x5678}
	id, err := filesystemIDFromStatfs(stat)
	if err != nil || id != "0000123400005678" {
		t.Fatalf("id=%q err=%v", id, err)
	}
	stat.Type = 0
	if _, err := filesystemIDFromStatfs(stat); err == nil {
		t.Fatal("accepted a non-Btrfs filesystem")
	}
}

func TestWorkspaceMountBoundaries(t *testing.T) {
	for _, tc := range []struct {
		mount    string
		conflict bool
	}{
		{"/target", false},
		{"/target/work", true},
		{"/target/work/nested", true},
		{"/target/group", false},
		{"/target/worker", false},
		{"/elsewhere", false},
	} {
		if got := mountConflicts("/target", "/target/work", tc.mount); got != tc.conflict {
			t.Errorf("mount %q conflict=%t, want %t", tc.mount, got, tc.conflict)
		}
	}
	if !mountConflicts("/target", "/target/group/work", "/target/group") {
		t.Fatal("mounted ancestor accepted")
	}
}
