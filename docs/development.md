# Development and verification

## Structure and build

Module `gobackupper`, Go 1.23+. The source is Linux-only: it uses process groups, flock, Waitid(WNOWAIT), and renameat2(RENAME_NOREPLACE). Windows/macOS portability is not claimed.

```sh
mkdir -p bin
go build -o bin/gobackupper ./cmd/gobackupper
bin/gobackupper help
go test ./...
go test -race ./...
go vet ./...
```

Format with `gofmt -w cmd internal`. The only external Go modules are the pinned `golang.org/x/sys` and `golang.org/x/term`; INI parsing, CLI flags, and process management use the standard library. Runtime executables rsync and btrfs are installed separately, not vendored. A Git repository is not required and was not initialized during implementation.

See [AGENTS.md](../AGENTS.md) for component ownership and required rules, and [DESIGN.md](../DESIGN.md) for architecture.

## Contracts

- `config.Load(filename, invocationDir)` returns a Plan and warnings. Sections and ordering are preserved; the planner checks filesystem availability for selected jobs.
- `backup.Select` preserves declaration order. `backup.Validate` does not mutate the filesystem.
- `backup.Service` accepts consumer-side Synchronizer/Repository interfaces and separate phase/progress callbacks. A phase callback may write UI; a progress callback must quickly update latest state.
- `process.Runner` owns the complete process-group lifecycle. Its stdout writer must promptly accept bytes and must not write to a slow UI. Run returns bounded diagnostics and errors that preserve cause/status.
- `rsync.Client` never creates the destination. Do not silently enable partial/inplace or additional metadata flags.
- `btrfs.Repository` owns storage locks, subvolumes/compression, and daily rotation. It performs no deletion, rollback, or retention.
- `ui.Renderer` separates state and output mutexes and discards frames from an old phase.

Configuration files are local trusted input. Do not expose the loader as an unrestricted public network service: its parser allows large values, and paths authorize filesystem operations by the application.

## Tests

| Package | Coverage |
|---|---|
| config | Multiple roots, ordering and job replacement, path resolution, DEFAULT/overrides, interpolation/%%/cycles, multiline/blank lines, case, duplicate errors, optional configs, delete/exclude, unused-section interpolation |
| pathresolve | Symlinks before parent traversal, missing suffixes |
| backup | Explicit selection, unsafe layouts/overlaps/prefix collisions, symlink roots/leaves, no snapshot after sync failure/cancellation, prepare all selected jobs before copying, partial success and date after Sync |
| process | Large stdout/stderr, bounded tails, exit/cause, process-group cancellation and TERM-ignoring descendants with closed pipes |
| rsync | CR/LF/chunks/EOF/overflow, filenames/single octal decoding, suffix counters; actual content, delete, exclude, symlink and nonzero result when rsync is installed |
| btrfs | CLI model: daily rotation/suffix gaps, retained previous snapshot on failure, compression failure, ordinary/symlink leaves, ro metadata, locks/partial release, mount boundaries |
| ui | Inert controls/invalid UTF-8, plain output, no false success at 100%, slow output does not block progress, stale-frame rejection |
| cmd/gobackupper | Help/usage, required selection, no-mutation validate with missing dependency, signal exit mapping |

Tests do not read home configs, touch real backup directories, require root/mount, or format a filesystem. Config tests that change cwd are not run in parallel. Rsync tests explicitly skip if the executable is absent; a skip is not evidence of runtime behavior.

The test Btrfs CLI is a model of adapter decisions, not proof of Btrfs atomicity/semantics. It operates only in TempDir and is explicitly not represented as kernel integration.

## Acceptance completed for the first implementation

Verified with Go 1.23.0, rsync 3.5.1, and installed btrfs-progs 6.6.3:

- `go test ./...` — all eight packages passed.
- `go test -race ./...` — all eight packages passed without race-detector reports.
- `go vet ./...` — no diagnostics.
- Built CLI: help and real read-only validation against an unsuitable target (tmpfs) — exited 3 without creating files.
- Built CLI + **real rsync** + temporary distributed INI + controlled Btrfs CLI: repeated successful runs, old/new content, delete=false and TRUE, excludes, symlink, and daily `.0`/`.1` rotation.
- In a real PTY (180 columns), live percentages were observed during transfer over ordinary rsync pipes; ESC in a filename was not emitted as a terminal command.
- SIGTERM during transfer exited with code 143; the daily snapshot was neither renamed nor newly created. A permanent process-group test also checks a TERM-ignoring descendant.

The PTY smoke used a wrapper that invoked real rsync with `--bwlimit=1024` so the transfer lasted long enough to observe; this option is not part of the user-facing CLI. The Btrfs helper modeled copy/snapshot metadata only inside TempDir. All temporary scripts, binaries, and fixtures were removed after the smoke run.

**Not verified:** actual kernel Btrfs snapshots/read-only/compression, real ENOSPC/permission failures during snapshotting, different mounts on one Btrfs filesystem, `-x` with a separate test mount, and other rsync versions. Temporary paths were on tmpfs, and access to the underlying Btrfs device was restricted. No dedicated Btrfs test filesystem was provided. User storage, sudo, mkfs, mount, and loop devices were not used.

## Acceptance on an explicitly provided Btrfs test filesystem

Only after safe test storage has been explicitly allocated:

1. Create independent source/target directories and a separate executable/config; use validate to check the resolved paths.
2. Include a regular file, nested directory, symlink, excluded file, and files that will be changed and deleted.
3. First run: inspect workspace contents, subvolume metadata, compression, and `ro=true` snapshot with the real btrfs CLI.
4. Change the source and run again with delete=false: verify the old snapshot and `.0` remain intact, the daily name has current contents, and stale workspace data remains.
5. Repeat with delete=true: workspace/daily reflect deletion; `.0`/`.1` are unchanged; suffix gaps are not overwritten.
6. Check failure/cancellation does not create a later snapshot; after failure following rename, the previous snapshot is available at the reported suffix.
7. With sufficient permissions, separately test different mounts on the same filesystem and `-x` on a dedicated test mount. Do not claim these checks without that environment.
8. Remove test subvolumes only as a separately authorized operation after verifying the exact path list. Never run cleanup against a user repository.

There is no public plugin/remote-engine API, and mocked tests alone do not establish production readiness. Behavioral changes require regression tests and updated user instructions.
