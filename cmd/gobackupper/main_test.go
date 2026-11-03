package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

func TestCLIUsageAndExplicitSelection(t *testing.T) {
	for _, args := range [][]string{nil, {"run"}, {"run", "--all", "--job", "A"}, {"unknown"}, {"run", "--all", "unexpected"}} {
		var out, errOut bytes.Buffer
		if code := execute(context.Background(), args, &out, &errOut, t.TempDir()); code != 2 {
			t.Fatalf("%v: code=%d error=%s", args, code, errOut.String())
		}
	}
	var out, errOut bytes.Buffer
	if code := execute(context.Background(), []string{"help"}, &out, &errOut, t.TempDir()); code != 0 || !strings.Contains(out.String(), "validate") {
		t.Fatalf("help: %d %q", code, out.String())
	}
}
func TestInvocationDirUsesSymlinkPath(t *testing.T) {
	root := t.TempDir()
	targetDir := filepath.Join(root, "target")
	launchDir := filepath.Join(root, "launch")
	for _, dir := range []string{targetDir, launchDir} {
		if err := os.Mkdir(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	target := filepath.Join(targetDir, "gobackupper")
	if err := os.WriteFile(target, []byte("binary"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(launchDir, "gobackupper")); err != nil {
		t.Fatal(err)
	}
	want, err := filepath.Abs(launchDir)
	if err != nil {
		t.Fatal(err)
	}

	t.Setenv("PATH", launchDir)
	got, err := invocationDir("gobackupper")
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("invocationDir() = %q, want symlink directory %q", got, want)
	}

	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.Chdir(cwd); err != nil {
			t.Errorf("restore working directory: %v", err)
		}
	})
	if err := os.Chdir(launchDir); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", ".")
	got, err = invocationDir("gobackupper")
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("invocationDir() with relative PATH = %q, want %q", got, want)
	}
}

func TestInvocationDirResolvesSymlinkBeforeParentTraversal(t *testing.T) {
	root := t.TempDir()
	realDir := filepath.Join(root, "real")
	subDir := filepath.Join(realDir, "sub")
	if err := os.MkdirAll(subDir, 0700); err != nil {
		t.Fatal(err)
	}
	launcher := filepath.Join(realDir, "gobackupper")
	if err := os.WriteFile(launcher, []byte("binary"), 0700); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(root, "alias")
	if err := os.Symlink(subDir, alias); err != nil {
		t.Fatal(err)
	}

	got, err := invocationDir(alias + "/../gobackupper")
	if err != nil {
		t.Fatal(err)
	}
	if got != realDir {
		t.Fatalf("invocationDir() = %q, want symlink-aware parent %q", got, realDir)
	}
	t.Setenv("PATH", alias+"/..")
	got, err = invocationDir("gobackupper")
	if err != nil {
		t.Fatal(err)
	}
	if got != realDir {
		t.Fatalf("invocationDir() from PATH = %q, want symlink-aware parent %q", got, realDir)
	}
}

func TestInvocationDirPreservesSymlinkedLauncherDirectory(t *testing.T) {
	root := t.TempDir()
	realDir := filepath.Join(root, "real")
	if err := os.Mkdir(realDir, 0700); err != nil {
		t.Fatal(err)
	}
	launcher := filepath.Join(realDir, "gobackupper")
	if err := os.WriteFile(launcher, []byte("binary"), 0700); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(root, "alias")
	if err := os.Symlink(realDir, alias); err != nil {
		t.Fatal(err)
	}
	want, err := filepath.Abs(alias)
	if err != nil {
		t.Fatal(err)
	}

	got, err := invocationDir(filepath.Join(alias, "gobackupper"))
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("invocationDir() = %q, want launcher directory %q", got, want)
	}
}

func TestFlagParseErrorsUseConsoleFormatter(t *testing.T) {
	var out, errOut bytes.Buffer
	code := execute(context.Background(), []string{"validate", "--unknown"}, &out, &errOut, t.TempDir())
	if code != 2 || !strings.Contains(errOut.String(), "Error: flag provided but not defined: -unknown") {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, out.String(), errOut.String())
	}
	if !strings.HasPrefix(errOut.String(), "Error:") || strings.Contains(errOut.String(), "Usage:") {
		t.Fatalf("parse error was not presented cleanly: %q", errOut.String())
	}
	if strings.ContainsRune(errOut.String(), '\x1b') {
		t.Fatalf("redirected diagnostic contains ANSI: %q", errOut.String())
	}
}

func TestValidateDoesNotCreateStorageWhenDependencyIsMissing(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "target")
	source := filepath.Join(root, "source")
	for _, p := range []string{target, source} {
		if err := os.Mkdir(p, 0700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, "config.ini"), []byte("[target]\npath=target\nsnapshots=target/Snapshots\n[sources]\nlocal="+source+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, ".backup.ini"), []byte("[targets]\nfirst=Docs\n[Docs]\npath=.\ntargetpath=docs\n"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", filepath.Join(root, "empty-path"))
	var out, errOut bytes.Buffer
	code := execute(context.Background(), []string{"validate"}, &out, &errOut, root)
	if code != 3 || !strings.Contains(errOut.String(), "rsync") {
		t.Fatalf("validate=%d out=%q error=%q", code, out.String(), errOut.String())
	}
	if !strings.Contains(out.String(), "Docs:") {
		t.Fatalf("distributed job not loaded: %q", out.String())
	}
	entries, err := os.ReadDir(target)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("validate created files: %v", entries)
	}
}

func TestSignalExitMapping(t *testing.T) {
	ctx, cancel := context.WithCancelCause(context.Background())
	cancel(signalCause{syscall.SIGTERM})
	if cancellationCode(ctx) != 143 {
		t.Fatal("SIGTERM code")
	}
	ctx, cancel = context.WithCancelCause(context.Background())
	cancel(signalCause{os.Interrupt})
	if cancellationCode(ctx) != 130 {
		t.Fatal("SIGINT code")
	}
}
