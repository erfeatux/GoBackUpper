package backup

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"gobackupper/internal/pathresolve"
)

// Select returns jobs in declaration order, regardless of flag order.
func Select(plan Plan, names []string, all bool) (Plan, error) {
	if all && len(names) != 0 {
		return Plan{}, fmt.Errorf("--all and --job cannot be combined")
	}
	if !all && len(names) == 0 {
		return Plan{}, fmt.Errorf("select --all or at least one --job")
	}
	wanted := make(map[string]bool, len(names))
	for _, name := range names {
		if wanted[name] {
			return Plan{}, fmt.Errorf("job %q selected twice", name)
		}
		wanted[name] = true
	}
	out := Plan{TargetRoot: plan.TargetRoot, SnapshotRoot: plan.SnapshotRoot}
	for _, job := range plan.Jobs {
		if all || wanted[job.Name] {
			out.Jobs = append(out.Jobs, job)
			delete(wanted, job.Name)
		}
	}
	if len(wanted) != 0 {
		for _, name := range names {
			if wanted[name] {
				return Plan{}, fmt.Errorf("unknown job %q", name)
			}
		}
	}
	if len(out.Jobs) == 0 {
		return Plan{}, fmt.Errorf("no sources found")
	}
	return out, nil
}

// canonical resolves symlinks in existing parents without requiring a missing leaf.
func canonical(path string) (string, error) {
	if !filepath.IsAbs(path) {
		return "", fmt.Errorf("path must be absolute: %q", path)
	}
	return pathresolve.Real(path)
}

func within(root, path string) bool {
	rel, err := filepath.Rel(root, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
}
func overlaps(a, b string) bool { return within(a, b) || within(b, a) }

func directory(path string) error {
	st, err := os.Stat(path)
	if err != nil {
		return err
	}
	if !st.IsDir() {
		return fmt.Errorf("%q is not a directory", path)
	}
	return nil
}

// noManagedSymlink forbids redirection in a managed tree, even to another safe path.
func noManagedSymlink(root, path string) error {
	for p := filepath.Clean(path); p != root; p = filepath.Dir(p) {
		st, err := os.Lstat(p)
		if err != nil && !os.IsNotExist(err) {
			return err
		}
		if err == nil && st.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("managed path is a symlink: %q", p)
		}
		if filepath.Dir(p) == p {
			return fmt.Errorf("%q escapes %q", path, root)
		}
	}
	return nil
}

// Validate checks the selected plan without changing files or invoking commands.
// Storage metadata is checked separately by Repository.Check.
func Validate(plan Plan) error {
	if len(plan.Jobs) == 0 {
		return fmt.Errorf("no selected jobs")
	}
	target, err := canonical(plan.TargetRoot)
	if err != nil {
		return fmt.Errorf("target: %w", err)
	}
	if target != filepath.Clean(plan.TargetRoot) {
		return fmt.Errorf("target root must be canonical without symlink components")
	}
	if err := directory(target); err != nil {
		return fmt.Errorf("target: %w", err)
	}
	snapshots, err := canonical(plan.SnapshotRoot)
	if err != nil {
		return fmt.Errorf("snapshots: %w", err)
	}
	if st, e := os.Lstat(plan.SnapshotRoot); e == nil {
		if st.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("snapshot root is a symlink")
		}
		if !st.IsDir() {
			return fmt.Errorf("snapshot root is not a directory")
		}
	} else if !os.IsNotExist(e) {
		return e
	} else if e = directory(filepath.Dir(plan.SnapshotRoot)); e != nil {
		return fmt.Errorf("snapshot parent: %w", e)
	}
	destinations := make([]string, len(plan.Jobs))
	sources := make([]string, len(plan.Jobs))
	prefixes := make(map[string]string, len(plan.Jobs))
	names := make(map[string]bool, len(plan.Jobs))
	for i, job := range plan.Jobs {
		if job.Name == "" || names[job.Name] {
			return fmt.Errorf("empty or duplicate job name %q", job.Name)
		}
		names[job.Name] = true
		if job.SnapshotPrefix == "" || strings.ContainsAny(job.SnapshotPrefix, "/\x00") || len(job.SnapshotPrefix)+len("-2006.01.02.18446744073709551615") > 255 {
			return fmt.Errorf("job %q has invalid snapshot prefix", job.Name)
		}
		if prev, ok := prefixes[job.SnapshotPrefix]; ok {
			return fmt.Errorf("jobs %q and %q share snapshot prefix %q", prev, job.Name, job.SnapshotPrefix)
		}
		prefixes[job.SnapshotPrefix] = job.Name
		source, err := canonical(job.SourcePath)
		if err != nil {
			return fmt.Errorf("source %q: %w", job.Name, err)
		}
		if err := directory(source); err != nil {
			return fmt.Errorf("source %q: %w", job.Name, err)
		}
		dest, err := canonical(job.Destination)
		if err != nil {
			return fmt.Errorf("destination %q: %w", job.Name, err)
		}
		if dest == target || !within(target, dest) {
			return fmt.Errorf("destination of %q must be a strict child of target", job.Name)
		}
		if err := noManagedSymlink(filepath.Clean(plan.TargetRoot), filepath.Clean(job.Destination)); err != nil {
			return fmt.Errorf("destination %q: %w", job.Name, err)
		}
		if err := directory(filepath.Dir(job.Destination)); err != nil {
			return fmt.Errorf("destination parent %q: %w", job.Name, err)
		}
		if snapshots != target && overlaps(dest, snapshots) {
			return fmt.Errorf("destination %q overlaps snapshot storage", job.Name)
		}
		sources[i], destinations[i] = source, dest
	}
	for i, source := range sources {
		if overlaps(source, snapshots) {
			return fmt.Errorf("source %q overlaps snapshot storage", plan.Jobs[i].Name)
		}
		for j, dest := range destinations {
			if overlaps(source, dest) {
				return fmt.Errorf("source %q overlaps destination %q", plan.Jobs[i].Name, plan.Jobs[j].Name)
			}
			if j > i && overlaps(destinations[i], dest) {
				return fmt.Errorf("destinations %q and %q overlap", plan.Jobs[i].Name, plan.Jobs[j].Name)
			}
		}
	}
	if snapshots == target {
		// Snapshot leaves must not collide with the first component of a workspace.
		for _, dest := range destinations {
			rel, _ := filepath.Rel(target, dest)
			first := strings.Split(rel, string(filepath.Separator))[0]
			for prefix := range prefixes {
				pattern := "^" + regexp.QuoteMeta(prefix) + `-\d{4}\.\d{2}\.\d{2}(\.\d+)?$`
				if regexp.MustCompile(pattern).MatchString(first) {
					return fmt.Errorf("workspace %q can collide with a daily snapshot", dest)
				}
			}
		}
	}
	return nil
}
