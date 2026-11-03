# Running jobs, progress, and errors

## CLI

```sh
gobackupper help
gobackupper validate [-c FILE] [--all | --job NAME ...]
gobackupper run [-c FILE] (--all | --job NAME ...)
```

Flags follow the command. Quote section names containing spaces in the shell: `--job 'Home Documents'`. Multiple `--job` flags select jobs in INI order, not flag order. Repeated selections, unknown names, using `--all` together with `--job`, and extra positional arguments are errors.

Without `--config`, `config.ini` is searched for next to the invocation path (the symlink directory when launched through a symlink). `validate` without a selection checks all jobs. `run` without a selection refuses to start: accidental EOF or a missing terminal is not treated as consent to copy everything.

Primary paths and the delete flag are printed to stdout. Errors, warnings, phases, and progress go to stderr. Interactive terminals use ANSI colors; redirected output contains no ANSI, and setting `NO_COLOR` disables color. Process diagnostics show the command, exit code, and output on separate indented lines. Names and diagnostics escape control characters and invalid UTF-8.

## What validate does

It reads both INI levels, builds the selected plan, and checks path existence and overlap, rsync presence and version, Btrfs CLI availability, filesystem identity, and metadata for existing destinations. Reading the Btrfs `ro` property confirms that the destination is a subvolume and whether it is writable or read-only. Ordinary directories are rejected as incompatible. Unexpected nested mounts in the workspace are also rejected. If Btrfs denies permission to read the property, `validate` prints a warning and continues with limited validation.

`validate` does not acquire locks, create directories or subvolumes, change compression, or copy data. A permission warning means the `ro` property could not be read, so subvolume status and writability are not confirmed. `run` repeats the check strictly and fails if it cannot verify the workspace property. Neither command proves that all future operations will be permitted or protects against changes between validation and execution.

## Run sequence

1. Load INI files and select jobs.
2. Build a safe plan and check dependencies and storage.
3. Acquire an exclusive nonblocking target lock, create a missing snapshot root with one `mkdir`, then acquire a second snapshots lock (or just one lock if the roots are identical).
4. Prepare **all selected** subvolumes; set compression on new ones, but do not reconfigure existing ones.
5. In sequence: rsync → local date → rename the prior daily snapshot → create a new read-only snapshot.
6. The first error stops the remaining jobs. Snapshots from earlier successful jobs are retained.
7. Wait for child processes to finish and release locks. Lock files remain on disk; their mere presence does not indicate a running process.

Deleting a lock file while a process is running is unsafe: another process could acquire a different inode. Do not remove locks to “unlock” the application. `flock` is released automatically when the descriptor closes or the process exits.

There is no automatic sudo, mounting, formatting, retrying, or history deletion. Avoid concurrent external rsync processes or other writers against the same directories; application locks coordinate GoBackUpper runs only.

## Understanding progress

Phases are prepare, sync, snapshot, and success. In a TTY, the display updates about five times per second; in a pipe, it emits ordinary, infrequent lines. The UI does not block rsync's stdout reader: it receives the latest state, and intermediate samples do not accumulate.

- `estimate` means rsync may still discover new files (`ir-chk`). The percentage can go down.
- `rsync` after `to-chk` is the percentage reported by rsync, **not the percentage of the entire backup**.
- Bytes/rate describe rsync transfer or reconstructed data, not physical writes or network speed.
- The current name is the latest matching transfer event, not proof that a file was saved.
- Elapsed time is calculated by the application. Rsync's human-readable time field is not always an ETA and is not used as a completion-time promise.
- A short or no-op sync may finish without intermediate samples.
- `progress incomplete` means a telemetry record was lost or too long; it does not indicate sync success or failure. Exit status remains authoritative.

100% does not mean metadata handling, deletions, the process, or snapshot creation is complete. Only `Saved ...` and successful command completion confirm a snapshot. An error after 100% is still an error.

## Cancellation

Ctrl-C/SIGINT and SIGTERM cancel the context. The active Linux process group receives TERM, then KILL after a grace period; the application waits for the command lifecycle and releases locks. Already-created successful snapshots are not deleted. If cancellation coincides with snapshot creation, the kernel operation may have completed: inspect the directory rather than deleting anything automatically.

If rsync is canceled, the work subvolume may be partial; the source and old snapshots are not rolled back or modified by the application. A hard SIGKILL to the application prevents guaranteed cleanup of external processes; on the next run, check for external writers.

## Exit codes

| Code | Meaning |
|---|---|
| 0 | Success / help |
| 2 | CLI, configuration read/schema, or unknown job error |
| 3 | Plan, dependency, storage, preparation, lock, or release error |
| 4 | Sync error |
| 5 | Rotation/snapshot creation error |
| 130 | SIGINT / context cancellation |
| 143 | SIGTERM |

The rsync/Btrfs command exit status is retained in diagnostics. Any nonzero rsync result, including 23/24, stops the workflow. Diagnostic tails are limited to 64 KiB per stream; truncation is indicated. `validate` may warn and continue when a permission restriction prevents checking metadata; `run` treats the same inability as an error.

## Common problems

- **Wrong target:** check the path bases in [configuration.md](configuration.md); `--config` does not change them.
- **Btrfs permission denied:** `validate` warns when access to subvolume/property metadata is denied; the named check remains unverified. `run` requires access to the metadata and fails rather than proceeding with an unverified workspace.
- **workspace is not a subvolume:** do not use an ordinary directory as the work target. The application does not delete existing data; choose an unused `targetpath` or manually prepare valid storage after independently checking the data.
- **lock busy:** wait for the other run; do not delete the file or bypass the lock with rsync.
- **snapshot on another FS:** configure snapshots on the same Btrfs filesystem; different mount points on the same filesystem are fine.
- **compression failure:** the new subvolume may already exist. Fix the setting or permissions manually; the application intentionally does not reconfigure existing subvolumes.
