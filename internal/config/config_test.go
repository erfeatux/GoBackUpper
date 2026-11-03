package config

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestLoadConfigLayoutAndMerge(t *testing.T) {
	cwd := t.TempDir()
	execDir := t.TempDir()
	changeDirectory(t, cwd)
	target := filepath.Join(execDir, "store")
	os.Mkdir(target, 0755)
	root1 := filepath.Join(cwd, "one")
	root2 := filepath.Join(cwd, "two")
	os.MkdirAll(filepath.Join(root1, "src"), 0755)
	os.MkdirAll(root2, 0755)
	writeConfig(t, filepath.Join(execDir, "config.ini"), "[target]\npath=/store\nsnapshots=/store/snaps\n[sources]\nfirst=one\nsecond=two\n")
	writeConfig(t, filepath.Join(root1, ".backup.ini"), "[targets]\na=Docs\nb=Other\n[Docs]\npath=src/\ntargetpath=machine/docs\ndelete=TRUE\nexclude=, cache,\n[Other]\npath=other\n")
	writeConfig(t, filepath.Join(root2, ".backup.ini"), "[targets]\nx=Docs\n[Docs]\npath=/var/lib/docs/\n")
	got, err := Load(filepath.Join(execDir, "config.ini"), execDir)
	if err != nil {
		t.Fatal(err)
	}
	if got.Plan.TargetRoot != target || got.Plan.SnapshotRoot != filepath.Join(target, "snaps") {
		t.Fatalf("roots: %#v", got.Plan)
	}
	if len(got.Plan.Jobs) != 2 {
		t.Fatalf("jobs: %#v", got.Plan.Jobs)
	}
	j := got.Plan.Jobs[0]
	if j.Name != "Docs" || j.SourcePath != "/var/lib/docs/" || j.Destination != target || j.SnapshotPrefix != "" || j.Delete || j.Compression != "zstd" {
		t.Fatalf("merged job: %#v", j)
	}
	if j.Excludes != nil {
		t.Fatalf("replacement retained excludes from previous definition: %#v", j.Excludes)
	}
	if got.Plan.Jobs[1].Name != "Other" || got.Plan.Jobs[1].SourcePath != filepath.Join(root1, "other") {
		t.Fatalf("relative job: %#v", got.Plan.Jobs[1])
	}
}

func TestLoadMissingOptionalAndBooleanSemantics(t *testing.T) {
	dir := t.TempDir()
	changeDirectory(t, dir)
	target := filepath.Join(dir, "target")
	os.Mkdir(target, 0755)
	root := filepath.Join(dir, "root")
	os.Mkdir(root, 0755)
	writeConfig(t, filepath.Join(dir, "config.ini"), "[target]\npath=target\nsnapshots=target/snaps\n[sources]\nmissing=absent\nroot=root\n")
	writeConfig(t, filepath.Join(root, ".backup.ini"), "[targets]\na=A\nb=B\nc=C\n[A]\npath=a\ndelete=true\nexclude=\n[B]\npath=b\ndelete=yes\n[C]\npath=c\ndelete=1\n")
	got, err := Load(filepath.Join(dir, "config.ini"), dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Warnings) != 1 {
		t.Fatalf("warnings: %#v", got.Warnings)
	}
	if !got.Plan.Jobs[0].Delete || got.Plan.Jobs[1].Delete || got.Plan.Jobs[2].Delete {
		t.Fatalf("boolean semantics: %#v", got.Plan.Jobs)
	}
	if !reflect.DeepEqual(got.Plan.Jobs[0].Excludes, []string{""}) {
		t.Fatalf("empty exclude: %#v", got.Plan.Jobs[0].Excludes)
	}
}

func TestParserInterpolationMultilineDuplicatesAndCycles(t *testing.T) {
	p := filepath.Join(t.TempDir(), "x.ini")
	writeConfig(t, p, "[DEFAULT]\nbase=alpha\nvalue=%(base)s %%\n[Case]\nkey=%(value)s\n  continued\n")
	f, _ := os.Open(p)
	parsed, err := parse(f)
	f.Close()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := parsed.readSection("Case"); err != nil {
		t.Fatal(err)
	}
	got, _ := parsed.sections["Case"].get("key")
	if got != "alpha %\ncontinued" {
		t.Fatalf("value %q", got)
	}
	for _, input := range []string{"[x]\na=1\nA=2\n", "[x]\n[x]\n", "[x]\na=%(b)s\nb=%(a)s\n"} {
		writeConfig(t, p, input)
		f, _ = os.Open(p)
		parsed, err = parse(f)
		f.Close()
		if err == nil {
			_, err = parsed.readSection("x")
		}
		if err == nil {
			t.Fatalf("expected parse error for %q", input)
		}
	}
}
func TestParserBlankLinesAndDefaultSectionCase(t *testing.T) {
	p := filepath.Join(t.TempDir(), "x.ini")
	writeConfig(t, p, "[DEFAULT]\nbase=one\n[default]\nvalue=literal\n[x]\nvalue=%(base)s\n\n  next\n")
	f, err := os.Open(p)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := parse(f)
	f.Close()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := parsed.sections["default"]; !ok {
		t.Fatal("[default] was treated as DEFAULT")
	}
	if _, err := parsed.readSection("x"); err != nil {
		t.Fatal(err)
	}
	value, _ := parsed.sections["x"].get("value")
	if value != "one\n\nnext" {
		t.Fatalf("blank continuation: %q", value)
	}
}

func TestParserIndentedHeaderIsContinuation(t *testing.T) {
	p := filepath.Join(t.TempDir(), "x.ini")
	writeConfig(t, p, "[x]\nvalue=first\n  continued\n  [not-a-section]\n[next] # trailing comment\nkey=value\n")
	f, err := os.Open(p)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := parse(f)
	f.Close()
	if err != nil {
		t.Fatal(err)
	}
	value, _ := parsed.sections["x"].get("value")
	if value != "first\ncontinued\n[not-a-section]" {
		t.Fatalf("continuation: %q", value)
	}
	if _, ok := parsed.sections["next"]; !ok {
		t.Fatal("following section missing")
	}
}

func TestParserDefaultOrderingAndRepeatedHeader(t *testing.T) {
	p := filepath.Join(t.TempDir(), "x.ini")
	writeConfig(t, p, "[DEFAULT]\na=one\nb=two\n[DEFAULT]\nc=three\n[x]\nb=override\nd=four\nformat=100%%\ncopy=%(format)s\n")
	f, err := os.Open(p)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := parse(f)
	f.Close()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := parsed.readSection("x"); err != nil {
		t.Fatal(err)
	}
	keys := []string{}
	for _, kv := range parsed.sections["x"].keys {
		keys = append(keys, kv.key)
	}
	want := []string{"a", "b", "c", "d", "format", "copy"}
	if !reflect.DeepEqual(keys, want) {
		t.Fatalf("key order %v, want %v", keys, want)
	}
	if got, _ := parsed.sections["x"].get("b"); got != "override" {
		t.Fatalf("override: %q", got)
	}
	if got, _ := parsed.sections["x"].get("copy"); got != "100%" {
		t.Fatalf("interpolation: %q", got)
	}
}

func TestParserRejectsOptionBeforeSection(t *testing.T) {
	p := filepath.Join(t.TempDir(), "x.ini")
	writeConfig(t, p, "option=value\n")
	f, err := os.Open(p)
	if err != nil {
		t.Fatal(err)
	}
	_, err = parse(f)
	f.Close()
	if err == nil {
		t.Fatal("expected option outside section error")
	}
}

func TestParserNoArbitraryLineLimit(t *testing.T) {
	p := filepath.Join(t.TempDir(), "x.ini")
	value := strings.Repeat("x", 2<<20)
	writeConfig(t, p, "[x]\nlarge="+value)
	f, err := os.Open(p)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := parse(f)
	f.Close()
	if err != nil {
		t.Fatal(err)
	}
	got, _ := parsed.sections["x"].get("large")
	if len(got) != len(value) {
		t.Fatalf("large value length %d, want %d", len(got), len(value))
	}
}

func TestUnusedInterpolationAndBlankLinesDoNotChangeJobs(t *testing.T) {
	dir := t.TempDir()
	writeConfig(t, filepath.Join(dir, "config.ini"), "[target]\npath=target\n\nsnapshots=target/Snapshots\n\n[sources]\nroot="+dir+"\n[unused]\nbad=%(missing)s\n")
	writeConfig(t, filepath.Join(dir, ".backup.ini"), "[targets]\na=A\n[A]\npath=src\n\n[unused]\nbad=%(missing)s\n")
	loaded, err := Load(filepath.Join(dir, "config.ini"), dir)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Plan.Jobs[0].SourcePath != filepath.Join(dir, "src") || loaded.Plan.TargetRoot != filepath.Join(dir, "target") {
		t.Fatalf("blank lines leaked into paths: %+v", loaded.Plan)
	}
}

// Relative [sources] are intentionally resolved against process cwd. These
// tests must not run in parallel with tests that change cwd or environment.
func changeDirectory(t *testing.T, dir string) {
	t.Helper()
	previous, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.Chdir(previous); err != nil {
			t.Error(err)
		}
	})
}
func writeConfig(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(strings.TrimSpace(contents)+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
}

func TestOptionalConfigMustBeRegularAndTargetTraversalIsRejected(t *testing.T) {
	dir := t.TempDir()
	optional := filepath.Join(dir, "optional")
	if err := os.MkdirAll(filepath.Join(optional, ".backup.ini"), 0700); err != nil {
		t.Fatal(err)
	}
	main := filepath.Join(dir, "config.ini")
	writeConfig(t, main, "[target]\npath=target\nsnapshots=target/Snapshots\n[sources]\noptional="+optional+"\nactual="+dir+"\n")
	actual := filepath.Join(dir, ".backup.ini")
	writeConfig(t, actual, "[targets]\na=A\n[A]\npath=src\n")
	loaded, err := Load(main, dir)
	if err != nil || len(loaded.Warnings) != 1 {
		t.Fatalf("optional directory: %+v %v", loaded, err)
	}
	writeConfig(t, actual, "[targets]\na=A\n[A]\npath=src\ntargetpath=group/../work\n")
	if _, err := Load(main, dir); err == nil {
		t.Fatal("parent traversal silently cleaned")
	}
}
