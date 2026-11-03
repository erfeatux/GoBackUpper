package btrfs

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/sys/unix"

	"gobackupper/internal/backup"
	"gobackupper/internal/process"
)

type Repository struct {
	plan               backup.Plan
	runner             process.Runner
	filesystemIDAtFunc func(string) (string, error)
	Binary             string
}

func New(plan backup.Plan, runner process.Runner) *Repository {
	return &Repository{plan: plan, runner: runner, Binary: "btrfs"}
}

func (r *Repository) binary() string {
	if r.Binary != "" {
		return r.Binary
	}
	return "btrfs"
}

func (r *Repository) command(ctx context.Context, args ...string) (process.Result, error) {
	return r.runner.Run(ctx, r.binary(), args, nil)
}

func (r *Repository) Check(ctx context.Context) error {
	return r.check(ctx, nil)
}

// Validate performs the non-mutating checks and reports checks skipped because
// the caller lacks permission to inspect Btrfs metadata.
func (r *Repository) Validate(ctx context.Context) ([]string, error) {
	var warnings []string
	err := r.check(ctx, func(warning string) {
		warnings = append(warnings, warning)
	})
	return warnings, err
}

func (r *Repository) check(ctx context.Context, onWarning func(string)) error {
	if _, err := r.command(ctx, "version"); err != nil {
		return fmt.Errorf("check btrfs executable: %w", err)
	}
	targetID, err := r.filesystemIDAt(ctx, r.plan.TargetRoot)
	if err != nil {
		return err
	}
	snapshotID, err := r.filesystemIDAt(ctx, r.plan.SnapshotRoot)
	if err != nil {
		return err
	}
	if targetID != snapshotID {
		return fmt.Errorf("target root and snapshot root belong to different Btrfs filesystems")
	}
	for _, job := range r.plan.Jobs {
		parent := filepath.Dir(job.Destination)
		st, err := os.Lstat(parent)
		if err != nil {
			return fmt.Errorf("inspect destination parent for %q: %w", job.Name, err)
		}
		if !st.IsDir() || st.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("destination parent for %q must be a real directory", job.Name)
		}
		parentID, err := r.filesystemIDAt(ctx, parent)
		if err != nil {
			return fmt.Errorf("check destination for %q: %w", job.Name, err)
		}
		if parentID != targetID {
			return fmt.Errorf("destination for %q is not on target Btrfs filesystem", job.Name)
		}
		if err := rejectNestedMountpoints(r.plan.TargetRoot, job.Destination); err != nil {
			return fmt.Errorf("check destination for %q: %w", job.Name, err)
		}
		if st, err := os.Lstat(job.Destination); err == nil {
			if !st.IsDir() || st.Mode()&os.ModeSymlink != 0 {
				return fmt.Errorf("workspace %q must be a real subvolume directory", job.Destination)
			}
			prop, err := r.command(ctx, "property", "get", job.Destination, "ro")
			if err != nil {
				if onWarning == nil || !isPermissionFailure(err) {
					return fmt.Errorf("verify workspace %q is writable: %w", job.Destination, err)
				}
				onWarning(fmt.Sprintf("Cannot verify workspace writable state; continuing with limited validation:\n%v", err))
			} else if !propertyValue(prop.Stdout, "ro", "false") {
				return fmt.Errorf("workspace %q is read-only or has unknown ro property", job.Destination)
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("inspect workspace %q: %w", job.Destination, err)
		}
	}
	return nil
}
func isPermissionFailure(err error) bool {
	if errors.Is(err, os.ErrPermission) {
		return true
	}
	var commandErr *process.Error
	if !errors.As(err, &commandErr) {
		return false
	}
	diagnostic := strings.ToLower(commandErr.Result.Stderr + "\n" + commandErr.Result.Stdout)
	return strings.Contains(diagnostic, "operation not permitted") ||
		strings.Contains(diagnostic, "permission denied")
}

func rejectNestedMountpoints(target, destination string) error {
	target, err := filepath.Abs(target)
	if err != nil {
		return err
	}
	destination, err = filepath.Abs(destination)
	if err != nil {
		return err
	}
	data, err := os.ReadFile("/proc/self/mountinfo")
	if err != nil {
		return fmt.Errorf("read mount metadata: %w", err)
	}
	decode := strings.NewReplacer("\\040", " ", "\\011", "\t", "\\012", "\n", "\\134", "\\")
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 5 {
			continue
		}
		mount := filepath.Clean(decode.Replace(fields[4]))
		if mountConflicts(target, destination, mount) {
			return fmt.Errorf("unexpected nested mountpoint %q", mount)
		}
	}
	return nil
}

func mountConflicts(target, destination, mount string) bool {
	if mount == target || !within(target, mount) {
		return false
	}
	return mount == destination || within(destination, mount) || within(mount, destination)
}

func within(parent, path string) bool {
	rel, err := filepath.Rel(parent, path)
	return err == nil && rel != "." && rel != ".." && !strings.HasPrefix(rel, ".."+string(os.PathSeparator))
}

func (r *Repository) filesystemIDAt(ctx context.Context, path string) (string, error) {
	probe := filepath.Clean(path)
	for {
		st, err := os.Lstat(probe)
		if err == nil {
			if !st.IsDir() || st.Mode()&os.ModeSymlink != 0 {
				return "", fmt.Errorf("storage path %q must be a real directory", probe)
			}
			identify := r.filesystemIDAtFunc
			if identify == nil {
				identify = filesystemIDAtPath
			}
			id, err := identify(probe)
			if err != nil {
				return "", fmt.Errorf("identify Btrfs filesystem for %q: %w", probe, err)
			}
			return id, nil
		}
		if !errors.Is(err, os.ErrNotExist) {
			return "", fmt.Errorf("inspect storage path %q: %w", probe, err)
		}
		next := filepath.Dir(probe)
		if next == probe {
			return "", fmt.Errorf("no existing parent for storage path %q", path)
		}
		probe = next
	}
}

func filesystemIDAtPath(path string) (string, error) {
	var stat unix.Statfs_t
	if err := unix.Statfs(path, &stat); err != nil {
		return "", err
	}
	return filesystemIDFromStatfs(stat)
}

func filesystemIDFromStatfs(stat unix.Statfs_t) (string, error) {
	if stat.Type != unix.BTRFS_SUPER_MAGIC {
		return "", fmt.Errorf("filesystem type 0x%x is not Btrfs", stat.Type)
	}
	return fmt.Sprintf("%08x%08x", uint32(stat.Fsid.Val[0]), uint32(stat.Fsid.Val[1])), nil
}

func (r *Repository) validatePath(ctx context.Context, path string) error {
	_, err := r.filesystemIDAt(ctx, path)
	return err
}

func (r *Repository) Acquire(ctx context.Context) (func() error, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	target, err := filepath.EvalSymlinks(r.plan.TargetRoot)
	if err != nil {
		return nil, fmt.Errorf("canonicalize target root: %w", err)
	}
	fds := make([]*os.File, 0, 2)
	unlock := func() error {
		var first error
		for i := len(fds) - 1; i >= 0; i-- {
			if e := unix.Flock(int(fds[i].Fd()), unix.LOCK_UN); e != nil && first == nil {
				first = e
			}
			if e := fds[i].Close(); e != nil && first == nil {
				first = e
			}
		}
		fds = nil
		return first
	}
	lock := func(path string) error {
		fd, e := unix.Open(path, unix.O_CREAT|unix.O_RDWR|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0600)
		if e != nil {
			return fmt.Errorf("open storage lock %q: %w", path, e)
		}
		f := os.NewFile(uintptr(fd), path)
		fds = append(fds, f)
		if e = unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB); e != nil {
			return fmt.Errorf("acquire storage lock %q: %w", path, e)
		}
		return nil
	}
	if err := lock(filepath.Join(target, ".gobackupper.lock")); err != nil {
		_ = unlock()
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		_ = unlock()
		return nil, err
	}
	if err := os.Mkdir(r.plan.SnapshotRoot, 0755); err != nil && !errors.Is(err, os.ErrExist) {
		_ = unlock()
		return nil, fmt.Errorf("create snapshot root %q: %w", r.plan.SnapshotRoot, err)
	}
	if err := r.validatePath(ctx, r.plan.SnapshotRoot); err != nil {
		_ = unlock()
		return nil, err
	}
	snapshot, err := filepath.EvalSymlinks(r.plan.SnapshotRoot)
	if err != nil {
		_ = unlock()
		return nil, fmt.Errorf("canonicalize snapshot root: %w", err)
	}
	if target != snapshot {
		if err := lock(filepath.Join(snapshot, ".gobackupper.lock")); err != nil {
			_ = unlock()
			return nil, err
		}
	}
	if err := r.Check(ctx); err != nil {
		_ = unlock()
		return nil, err
	}
	return unlock, nil
}

var _ backup.Repository = (*Repository)(nil)

func (r *Repository) Prepare(ctx context.Context, job backup.Job) (backup.Workspace, error) {
	if err := ctx.Err(); err != nil {
		return backup.Workspace{}, err
	}
	st, err := os.Lstat(job.Destination)
	if errors.Is(err, os.ErrNotExist) {
		if _, e := r.command(ctx, "subvolume", "create", job.Destination); e != nil {
			return backup.Workspace{}, fmt.Errorf("create workspace for %q: %w", job.Name, e)
		}
		if _, e := r.command(ctx, "property", "set", job.Destination, "compression", job.Compression); e != nil {
			return backup.Workspace{}, fmt.Errorf("set compression for %q: %w", job.Name, e)
		}
		return backup.Workspace{Job: job}, nil
	}
	if err != nil {
		return backup.Workspace{}, fmt.Errorf("inspect workspace %q: %w", job.Destination, err)
	}
	if !st.IsDir() || st.Mode()&os.ModeSymlink != 0 {
		return backup.Workspace{}, fmt.Errorf("workspace %q is not a real subvolume directory", job.Destination)
	}
	prop, err := r.command(ctx, "property", "get", job.Destination, "ro")
	if err != nil {
		return backup.Workspace{}, fmt.Errorf("verify writable workspace %q: %w", job.Destination, err)
	}
	if !propertyValue(prop.Stdout, "ro", "false") {
		return backup.Workspace{}, fmt.Errorf("existing workspace %q is read-only or has unknown ro property", job.Destination)
	}
	return backup.Workspace{Job: job}, nil
}
func propertyValue(output, key, value string) bool {
	for _, line := range strings.Split(output, "\n") {
		parts := strings.SplitN(strings.TrimSpace(line), "=", 2)
		if len(parts) == 2 && parts[0] == key && strings.EqualFold(parts[1], value) {
			return true
		}
	}
	return false
}

func (r *Repository) Snapshot(ctx context.Context, workspace backup.Workspace, date time.Time) (backup.Snapshot, error) {
	if err := ctx.Err(); err != nil {
		return backup.Snapshot{}, err
	}
	job := workspace.Job
	day := filepath.Join(r.plan.SnapshotRoot, job.SnapshotPrefix+"-"+date.In(time.Local).Format("2006.01.02"))
	var previous string
	st, statErr := os.Lstat(day)
	if statErr == nil {
		if st.Mode()&os.ModeSymlink != 0 || !st.IsDir() {
			return backup.Snapshot{}, fmt.Errorf("existing snapshot %q is not a subvolume directory", day)
		}
		prop, e := r.command(ctx, "property", "get", day, "ro")
		if e != nil {
			return backup.Snapshot{}, fmt.Errorf("verify read-only snapshot %q: %w", day, e)
		}
		if !propertyValue(prop.Stdout, "ro", "true") {
			return backup.Snapshot{}, fmt.Errorf("existing snapshot %q is not read-only", day)
		}
		for i := 0; ; i++ {
			candidate := fmt.Sprintf("%s.%d", day, i)
			_, e := os.Lstat(candidate)
			if errors.Is(e, os.ErrNotExist) {
				previous = candidate
				break
			}
			if e != nil {
				return backup.Snapshot{}, fmt.Errorf("inspect rotation destination %q: %w", candidate, e)
			}
		}
		if err := ctx.Err(); err != nil {
			return backup.Snapshot{Path: day}, err
		}
		if err := unix.Renameat2(unix.AT_FDCWD, day, unix.AT_FDCWD, previous, unix.RENAME_NOREPLACE); err != nil {
			return backup.Snapshot{Path: day}, fmt.Errorf("rotate snapshot %q to %q: %w", day, previous, err)
		}
	}
	if statErr != nil && !errors.Is(statErr, os.ErrNotExist) {
		return backup.Snapshot{}, fmt.Errorf("inspect daily snapshot %q: %w", day, statErr)
	}
	if err := ctx.Err(); err != nil {
		return backup.Snapshot{Path: day, PreviousPath: previous}, withPrevious(err, previous)
	}
	if _, err := r.command(ctx, "subvolume", "snapshot", "-r", job.Destination, day); err != nil {
		err = fmt.Errorf("create snapshot %q: %w", day, err)
		return backup.Snapshot{Path: day, PreviousPath: previous}, withPrevious(err, previous)
	}
	return backup.Snapshot{Path: day, PreviousPath: previous}, nil
}
func withPrevious(err error, path string) error {
	if path == "" {
		return err
	}
	return fmt.Errorf("%w (previous snapshot retained at %q)", err, path)
}
