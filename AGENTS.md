# GoBackUpper Development Instructions

## Accepted decisions

- Linux CLI written in Go. Do not initialize a Git repository unless explicitly asked.
- `DESIGN.md` is the architecture reference and documents current contracts and implementation decisions.
- Use the system rsync through `os/exec`. Use only the Btrfs CLI (`btrfs-progs`); do not add an rsync implementation/wrapper library, Btrfs ioctl library, TOML, Viper, a DI container, or a generic FileSystem hierarchy.
- Preserve both configuration layers: `config.ini` with `[target]`/`[sources]`, and distributed `.backup.ini` files with `[targets]` and named sections. Preserve order, DEFAULT/interpolation, path bases, true-only delete semantics, and comma-separated excludes. See DESIGN.md §3.
- Preserve existing destinations and daily snapshot names. After successful rsync, rename the previous daily snapshot to the first free `.0`, `.1`, etc.; create the new snapshot at the unsuffixed name. Use local date after rsync, separately for each job. Do not introduce UUID/timestamp naming, retention, or automatic rollback.
- A backup succeeds only when rsync exits successfully and a read-only snapshot is created. Progress is optional telemetry. On failure/cancellation, do not start later jobs.
- Never change compression on an existing subvolume. Failure to configure a newly created subvolume is a preparation failure, not success.
- Never test against users' backups, home configs, or storage devices. Do not run mkfs/mount/sudo or create loop devices. A real Btrfs smoke test is allowed only on a test filesystem explicitly provided for that purpose.

## Code map and dependencies

- `cmd/gobackupper/`: main, flags/subcommands, signal handling, dependency wiring, CLI exit mapping. Only this package calls `os.Exit`.
- `internal/backup/`: typed Plan/Job/Progress/Result, safe planning, sequential workflow; interfaces are owned by the consumer.
- `internal/config/`: INI parser/loader, provenance, ordered merge. No commands or terminal output.
- `internal/pathresolve/`: shared non-strict realpath for roots and relative sources; do not apply `filepath.Abs`/`Join`/`Clean` before resolving symlinks and `..`.
- `internal/process/`: one lifecycle for external processes, bounded output, stdout/stderr draining, process-group cancellation and reaping.
- `internal/rsync/`: rsync arguments, capability checks, CR/LF framing, progress/file decoder. Never creates destinations or snapshots.
- `internal/btrfs/`: storage checks, locks, subvolumes/compression, rename and read-only snapshots. Never parses config or invokes rsync.
- `internal/ui/`: safe TTY/plain output and state snapshots; must not block readers or manage child processes.
- `docs/`: detailed user documentation. `README.md` is the concise GitHub project overview and links to user guides.

## Go and quality rules

- Correctness and data safety before abstractions. Prefer small concrete types and explicit dependencies; use narrow interfaces only at real boundaries.
- Return contextual errors and wrap causes with `%w`; classify with `errors.Is`/`errors.As`. Never suppress process, pipe, compression, or snapshot errors.
- Never invoke a shell for user paths/arguments. Use absolute paths and argv slices. Set `LC_ALL=C` only for the child.
- Propagate context to external operations. Drain stdout/stderr concurrently and bound memory. Cancellation terminates the process group and reaps children. Do not impose an arbitrary timeout on the whole backup.
- Runner uses Linux `Waitid(WNOWAIT)`: keep the leader unreaped until the final signal is sent to its process group, then call `cmd.Wait`. Do not replace this with an early Wait followed by a group kill: the PID may be reused and TERM-ignoring descendants may outlive the leader.
- Do not invoke potentially blocking UI callbacks from stream readers. Keep only latest-state telemetry; do not queue unbounded progress samples. Escape controls/invalid UTF-8 in filenames and diagnostics. Emit ANSI only on an interactive terminal; honor `NO_COLOR` and keep redirected output plain.
- Do not pre-scan the whole tree just to estimate a percentage; never use progress percentages for storage decisions.
- Read relevant code first. Use LSP for definitions/references/rename when available; do not perform cross-file symbol changes with text replacement.
- Test consumer-visible behavior, boundaries, failures and transitions. Avoid tautologies, mock echoes and source-text assertions. Use `TempDir`, deterministic clocks, and isolation from unrelated mounts/configs.
- Tests that change cwd/environment must not use `t.Parallel`. INI interpolation occurs on section access: do not expand unused sections or mutate raw maps during expansion. Inline `#`/`;` are value text; exact `DEFAULT` is special; default/override key order matters.
- Delegated slices must not run build/test/vet/format mid-flight. One integrator runs final checks after integration.

## Verification and documentation

- Final checks: `gofmt` changed Go files, `go test ./...`, `go test -race ./...`, `go vet ./...`, build and run the CLI, and smoke with real rsync in a TempDir. If no Btrfs test filesystem is provided, state that plainly; fake commands do not prove real snapshots.
- Keep README and docs consistent with the actual CLI, including unusual path semantics, exit codes, deletion safety, restore, and consistency limits.
- Contract changes require code, behavioral tests, DESIGN/README/docs, and this file to be updated. Do not call behavior implemented before exercising the changed path.

## Implementation findings to retain

- The INI loader uses the standard library only; go-ini was considered but not added. Parser behavior is covered by Go tests.
- Reject any `..` component in managed `targetpath` before cleaning. Do not silently repair an empty default basename from an absolute source ending in `/`; source-relative paths use the shared realpath resolver.
- Do not open a `.backup.ini` that is not a regular file (directory/FIFO/etc.) as a stream. Warn and skip it as an optional config.
- UI state/output use separate mutexes. A delayed render rechecks its phase after acquiring the output mutex, so it cannot print sync progress after snapshot/success.
- Keep regressions for a mountpoint exactly at the destination, unresolved symlink target root, old snapshot unchanged on sync failure, and reporting the previous snapshot path if snapshot creation fails after rename.
- CLI: `run` requires `--all` or `--job`; `validate` defaults to all jobs and creates nothing. Permission-denied Btrfs metadata checks during `validate` produce a warning and limited validation; operational `Check` remains strict for `run`. CLI exit codes and selection behavior are defined by the current interface. See `docs/usage.md`.
- First implementation acceptance completed: unit/integration tests, race, vet, actual binary + rsync, plain/PTY output and SIGTERM. Real kernel Btrfs snapshots/compression were not tested; a controlled CLI model is not equivalent. Full record: `docs/development.md`.
