// GoBackUpper synchronizes local directories and records read-only Btrfs snapshots.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"gobackupper/internal/backup"
	"gobackupper/internal/btrfs"
	"gobackupper/internal/config"
	"gobackupper/internal/process"
	"gobackupper/internal/rsync"
	"gobackupper/internal/ui"
)

const usage = `GoBackUpper — local rsync backups with read-only Btrfs snapshots

Usage:
  gobackupper run [--config FILE] (--all | --job NAME [--job NAME ...])
  gobackupper validate [--config FILE] [--all | --job NAME ...]
  gobackupper help

Default config: config.ini next to the invocation path (the symlink path, if applicable).
validate checks all jobs by default, without creating directories or taking locks.
--job names are case-sensitive source section names from .backup.ini.
--config (or -c) uses the documented path resolution rules.
No scheduling, automatic snapshot deletion, mounting or privilege escalation.
`

type jobNames []string

func (v *jobNames) String() string     { return fmt.Sprint([]string(*v)) }
func (v *jobNames) Set(s string) error { *v = append(*v, s); return nil }

type signalCause struct{ signal os.Signal }

func (e signalCause) Error() string { return e.signal.String() }

func cancellationCode(ctx context.Context) int {
	var cause signalCause
	if errors.As(context.Cause(ctx), &cause) && cause.signal == syscall.SIGTERM {
		return 143
	}
	return 130
}

func reportError(out io.Writer, err error) { ui.NewConsole(out).Error(err) }

func printUsage(out io.Writer) {
	console := ui.NewConsole(out)
	for _, line := range strings.Split(usage, "\n") {
		switch {
		case strings.HasPrefix(line, "GoBackUpper"), line == "Usage:":
			line = console.Accent(line)
		case strings.HasPrefix(line, "  gobackupper"):
			line = console.Success(line)
		}
		fmt.Fprintln(out, line)
	}
}

func execute(ctx context.Context, args []string, out, errOut io.Writer, invocationDir string) int {
	outConsole := ui.NewConsole(out)
	errConsole := ui.NewConsole(errOut)
	if len(args) == 0 {
		printUsage(errOut)
		return 2
	}
	command := args[0]
	if command == "help" || command == "-h" || command == "--help" {
		printUsage(out)
		return 0
	}
	if command != "run" && command != "validate" {
		reportError(errOut, fmt.Errorf("unknown command %q", command))
		printUsage(errOut)
		return 2
	}
	flags := flag.NewFlagSet(command, flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	flags.Usage = func() {}
	filename := filepath.Join(invocationDir, "config.ini")
	flags.StringVar(&filename, "config", filename, "main INI file")
	flags.StringVar(&filename, "c", filename, "main INI file")
	all := flags.Bool("all", false, "select every declared source")
	var names jobNames
	flags.Var(&names, "job", "select source section by name (repeatable)")
	if err := flags.Parse(args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			printUsage(errOut)
			return 0
		}
		reportError(errOut, err)
		return 2
	}
	if flags.NArg() != 0 {
		reportError(errOut, fmt.Errorf("unexpected arguments: %q", flags.Args()))
		return 2
	}
	if command == "validate" && len(names) == 0 {
		*all = true
	}
	if command == "run" && !*all && len(names) == 0 {
		reportError(errOut, fmt.Errorf("run requires --all or --job"))
		return 2
	}
	if *all && len(names) != 0 {
		reportError(errOut, fmt.Errorf("--all and --job cannot be combined"))
		return 2
	}
	loaded, err := config.Load(filename, invocationDir)
	if err != nil {
		reportError(errOut, err)
		return 2
	}
	for _, warning := range loaded.Warnings {
		errConsole.Warning(warning)
	}
	plan, err := backup.Select(loaded.Plan, names, *all)
	if err != nil {
		reportError(errOut, err)
		return 2
	}
	if err := backup.Validate(plan); err != nil {
		reportError(errOut, err)
		return 3
	}
	fmt.Fprintf(out, "%s %s\n%s %s\n", outConsole.Accent("Target:"), ui.Safe(plan.TargetRoot), outConsole.Accent("Snapshots:"), ui.Safe(plan.SnapshotRoot))
	for _, job := range plan.Jobs {
		fmt.Fprintf(out, "%s: %s -> %s %s\n", outConsole.Accent(ui.Safe(job.Name)), ui.Safe(job.SourcePath), ui.Safe(job.Destination), outConsole.Muted(fmt.Sprintf("(delete=%t)", job.Delete)))
	}
	runner := process.Runner{}
	synchronizer := rsync.New(runner)
	repository := btrfs.New(plan, runner)
	if command == "validate" {
		if err := synchronizer.Check(ctx); err != nil {
			reportError(errOut, err)
			if ctx.Err() != nil {
				return cancellationCode(ctx)
			}
			return 3
		}
		warnings, err := repository.Validate(ctx)
		for _, warning := range warnings {
			errConsole.Warning(warning)
		}
		if err != nil {
			reportError(errOut, err)
			if ctx.Err() != nil {
				return cancellationCode(ctx)
			}
			return 3
		}
		fmt.Fprintf(out, "%s No files changed.\n", outConsole.Success("Validation passed."))
		return 0
	}
	renderer := ui.New(errOut)
	stop := renderer.Start(ctx)
	service := backup.Service{Sync: synchronizer, Repository: repository, OnEvent: renderer.Event, OnProgress: renderer.Progress}
	result, err := service.Run(ctx, plan)
	stop()
	if err != nil {
		reportError(errOut, err)
		fmt.Fprintf(errOut, "%s %d\n", errConsole.Muted("Completed snapshots before failure:"), len(result.Jobs))
		if ctx.Err() != nil {
			return cancellationCode(ctx)
		}
		var failure *backup.Error
		if errors.As(err, &failure) {
			switch failure.Stage {
			case backup.StageSync:
				return 4
			case backup.StageSnapshot:
				return 5
			}
		}
		return 3
	}
	fmt.Fprintf(out, "%s %d snapshot(s).\n", outConsole.Success("Completed:"), len(result.Jobs))
	return 0
}
func invocationDir(argv0 string) (string, error) {
	path, err := exec.LookPath(argv0)
	if err != nil && !errors.Is(err, exec.ErrDot) {
		return "", fmt.Errorf("resolve invocation path %q: %w", argv0, err)
	}
	path, err = filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("make invocation path absolute: %w", err)
	}
	return filepath.Dir(path), nil
}

func realMain() int {
	launchDir, err := invocationDir(os.Args[0])
	if err != nil {
		reportError(os.Stderr, err)
		return 2
	}
	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(signals)
	go func() {
		select {
		case sig := <-signals:
			cancel(signalCause{sig})
		case <-ctx.Done():
		}
	}()
	return execute(ctx, os.Args[1:], os.Stdout, os.Stderr, launchDir)
}

func main() { os.Exit(realMain()) }
