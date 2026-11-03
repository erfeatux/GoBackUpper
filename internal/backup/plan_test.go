package backup

import (
	"os"
	"path/filepath"
	"testing"
)

func testPlan(t *testing.T) Plan {
	t.Helper()
	root := t.TempDir()
	for _, dir := range []string{"target", "source-a", "source-b"} {
		if err := os.Mkdir(filepath.Join(root, dir), 0700); err != nil {
			t.Fatal(err)
		}
	}
	return Plan{TargetRoot: filepath.Join(root, "target"), SnapshotRoot: filepath.Join(root, "target", "Snapshots"), Jobs: []Job{
		{Name: "A", SourcePath: filepath.Join(root, "source-a"), Destination: filepath.Join(root, "target", "a"), SnapshotPrefix: "a", Compression: "zstd"},
		{Name: "B", SourcePath: filepath.Join(root, "source-b"), Destination: filepath.Join(root, "target", "b"), SnapshotPrefix: "b", Compression: "zstd"},
	}}
}

func TestSelectPreservesOrderAndDoesNotCheckUnselectedPaths(t *testing.T) {
	plan := testPlan(t)
	plan.Jobs[1].SourcePath = filepath.Join(t.TempDir(), "absent")
	selected, err := Select(plan, []string{"A"}, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := Validate(selected); err != nil {
		t.Fatal(err)
	}
	selected, err = Select(plan, []string{"B", "A"}, false)
	if err != nil {
		t.Fatal(err)
	}
	if selected.Jobs[0].Name != "A" || selected.Jobs[1].Name != "B" {
		t.Fatalf("order: %+v", selected.Jobs)
	}
	if err := Validate(selected); err == nil {
		t.Fatal("selected absent source accepted")
	}
	for _, names := range [][]string{nil, {"absent"}, {"A", "A"}} {
		if _, err := Select(plan, names, false); err == nil {
			t.Fatalf("invalid selection accepted: %v", names)
		}
	}
	if _, err := Select(plan, []string{"A"}, true); err == nil {
		t.Fatal("ambiguous selection accepted")
	}
}

func TestValidateRejectsDestructiveLayouts(t *testing.T) {
	cases := map[string]func(*Plan){
		"same destination":            func(p *Plan) { p.Jobs[1].Destination = p.Jobs[0].Destination },
		"nested destination":          func(p *Plan) { p.Jobs[1].Destination = filepath.Join(p.Jobs[0].Destination, "child") },
		"same prefix":                 func(p *Plan) { p.Jobs[1].SnapshotPrefix = p.Jobs[0].SnapshotPrefix },
		"target itself":               func(p *Plan) { p.Jobs[0].Destination = p.TargetRoot },
		"escape":                      func(p *Plan) { p.Jobs[0].Destination = filepath.Join(p.TargetRoot, "..", "escape") },
		"snapshot destination":        func(p *Plan) { p.Jobs[0].Destination = p.SnapshotRoot },
		"source contains destination": func(p *Plan) { p.Jobs[0].SourcePath = p.TargetRoot },
		"source is other destination": func(p *Plan) { p.Jobs[1].Destination = p.Jobs[0].SourcePath },
		"empty prefix":                func(p *Plan) { p.Jobs[0].SnapshotPrefix = "" },
		"slash prefix":                func(p *Plan) { p.Jobs[0].SnapshotPrefix = "a/b" },
		"snapshot parent missing":     func(p *Plan) { p.SnapshotRoot = filepath.Join(p.TargetRoot, "absent", "snapshots") },
	}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			plan := testPlan(t)
			change(&plan)
			if err := Validate(plan); err == nil {
				t.Fatal("unsafe plan accepted")
			}
		})
	}
}

func TestValidateRejectsManagedSymlinkEvenWhenItStaysInsideRoot(t *testing.T) {
	plan := testPlan(t)
	if err := os.Mkdir(filepath.Join(plan.TargetRoot, "real"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("real", plan.Jobs[0].Destination); err != nil {
		t.Fatal(err)
	}
	if err := Validate(plan); err == nil {
		t.Fatal("managed symlink accepted")
	}
}

func TestValidateAllowsSameRootsWithoutSnapshotNameCollision(t *testing.T) {
	plan := testPlan(t)
	plan.SnapshotRoot = plan.TargetRoot
	if err := Validate(plan); err != nil {
		t.Fatal(err)
	}
	plan.Jobs[0].Destination = filepath.Join(plan.TargetRoot, "a-2026.11.03")
	if err := Validate(plan); err == nil {
		t.Fatal("daily name collision accepted")
	}
}

func TestValidateRejectsUnresolvedSymlinkRoot(t *testing.T) {
	plan := testPlan(t)
	alias := filepath.Join(filepath.Dir(plan.TargetRoot), "target-alias")
	if err := os.Symlink(plan.TargetRoot, alias); err != nil {
		t.Fatal(err)
	}
	plan.TargetRoot = alias
	plan.SnapshotRoot = filepath.Join(alias, "Snapshots")
	plan.Jobs[0].Destination = filepath.Join(alias, "a")
	plan.Jobs[1].Destination = filepath.Join(alias, "b")
	if err := Validate(plan); err == nil {
		t.Fatal("unresolved target root redirected managed paths")
	}
}
