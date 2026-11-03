# GoBackUpper: Architecture of a Standalone Go Application

## 1. Decision

GoBackUpper synchronizes local directories into Btrfs work subvolumes and maintains dated, read-only snapshots. Configuration uses `config.ini` and distributed `.backup.ini` files. A successful run uses system rsync, then rotates the daily snapshot name and creates a new read-only snapshot.

The contracts in this document describe current behavior. See [docs/development.md](docs/development.md) for verification results and limitations.

**Implemented:** system rsync as the copy engine, a custom `os/exec` adapter for process execution and progress; the Btrfs CLI as the sole storage adapter; an INI loader with a typed internal model; a sequential application service; and a UI that observes state but does not control pipe reading. Code is in `cmd/` and `internal/`; acceptance results and limitations are in [docs/development.md](docs/development.md).

We do not rewrite synchronization algorithms, terminal detection, or Unix locks. The INI parser uses the standard library to implement the documented ordering, interpolation, and multiline rules. Our code handles backup rules, safe paths, configuration loading, rsync observation, and operation coordination.

## 2. Existing Components: What to Use and What Not to Use

| Area | Decision | Rationale and cost |
|---|---|---|
| Synchronization engine | System rsync | Retains a mature implementation of metadata, filters, delete, and filesystem traversal; executable must be installed |
| Process execution | Standard `os/exec`, `context`, `os/signal` | Full control of lifetime, argv, env, stdout/stderr; no wrapper library needed |
| INI | Custom loader using stdlib | Order, DEFAULT, lazy interpolation, and multiline behavior checked with fixtures; go-ini candidate not added, TOML/Viper not used |
| TTY/window size | `golang.org/x/term` | Avoid implementing terminal detection with ioctl ourselves |
| Linux process groups and lock | `golang.org/x/sys/unix` | Small Linux adapter for flock/signals; not a general distributed lock |
| CLI | Standard `flag.FlagSet` | Sufficient for `run` with explicit jobs selection, validate, and help; Cobra is not justified yet |
| Diagnostics | Contextual errors + safe CLI text | No separate structured logging pipeline for a local sequential command |
| Btrfs | System `btrfs` via the same process runner | Minimal custom kernel ABI code and familiar administrator diagnostics; external dependency is intentional |
| Progress bar | Small renderer over state + x/term | A bar does not solve statistics collection. Full-screen TUI/Bubble Tea is unnecessary for one operation |

`x/sys` and `x/term` versions are pinned in go.mod/go.sum; Go 1.23+. Do not import HEAD or choose a dependency by star count.

### 2.1. Is There an rsync in Go?

Yes: [gokrazy/rsync](https://github.com/gokrazy/rsync) is an independent client and server implementation, not a shell wrapper. [rsynccmd](https://github.com/gokrazy/rsync/blob/main/rsynccmd/rsynccmd.go) provides Command/Run(context), stdout/stderr, and the final Result.Stats. In the reviewed API, Cmd has no structured live-progress callback: switching to the library would not by itself solve progress.

The gokrazy implementation has been tested less thoroughly than the upstream rsync utility. For a backup product, avoiding an external executable does not outweigh the risk of differences in metadata, special files, filters, and deletion behavior. **Not selected.** We do not introduce a registry of multiple engines.

### 2.2. The grsync Wrapper Library

We reviewed the available [amilstead/grsync](https://github.com/amilstead/grsync), including [task.go](https://github.com/amilstead/grsync/blob/HEAD/task.go) and [rsync.go](https://github.com/amilstead/grsync/blob/HEAD/rsync.go). The zloylos/grsync URL returned 404 when checked; this does not establish that all forks are identical.

Specific reasons not to use the reviewed fork:

- `NewTask` forcibly enables Archive, Partial, HumanReadable, Progress: the library changes copy policy for the UI.
- “Percentage” = `(total - remain) / total` over the check-list, not the volume of copied data.
- Scanner splits on newlines, whereas intermediate progress uses carriage returns; Scanner errors are not checked.
- State/Log are read concurrently with goroutine writes without synchronization.
- All stdout/stderr is accumulated by string concatenation without a memory limit.
- Run does not separately wait for reader goroutines to finish; cancellation context is absent from the public launch API.
- Destination creation is hidden inside the rsync wrapper, violating Btrfs storage ownership.

This was source inspection, not a race test of the library. Adopting its architecture and fixing the fork would cost more to maintain than a narrow adapter for the options we need.

### 2.3. Btrfs: CLI Only

We use the installed `btrfs` from btrfs-progs through the shared process runner. We do not add `dennwc/btrfs`: a separate Go dependency for subvolumes/snapshots is unnecessary. There is no custom ioctl or alternative backend.

Operations: check filesystem/subvolume metadata, create subvolumes, set compression, and run `btrfs subvolume snapshot -r`. Before creating a snapshot, its final path must not exist: an existing directory must not be treated as a container for a nested snapshot. Success of every command is determined by exit status, including compression setup.

## 3. Configuration and Data Model

### 3.1. Files and Load Order

`config.ini` is located next to the path used to launch the executable; when launched through a symlink, its directory is used. Symlinks are resolved before later `..` components, matching filesystem traversal. `.backup.ini` files are loaded from each root listed in `[sources]`. An explicit `--config <path>` selects the primary INI; otherwise the adjacent `config.ini` is used. The invocation directory is also the base for `target.path` and `target.snapshots`.

```ini
; config.ini
[target]
path=backup
snapshots=backup/Snapshots

[sources]
local=/data
```

```ini
; /data/.backup.ini
[targets]
first=Documents
second=Photos

[Documents]
path=documents
targetpath=documents
delete=true
exclude=*.tmp, cache
compression=zstd

[Photos]
path=photos
```

Values in `[targets]` are section names and job identifiers (`Documents`, `Photos`); keys `first`/`second` are ordered references. We do not introduce a new ASCII-only job ID or derive storage paths from identifiers.

The loader retains root and target order. A repeated source name in another root replaces its definition without changing the position of its first occurrence. A missing `.backup.ini` is a warning and is skipped; having no sources at all is an error. Required sections and `path` are checked at load time. Directory availability and plan safety checks are performed for selected jobs.

### 3.2. Field and Path Semantics

| Field | Behavior |
|---|---|
| `target.path`, `target.snapshots` | Required; resolved from `invocationDir + "/" + value`. Even a leading `/` in the value does not cancel the invocation-directory prefix |
| `[sources].<key>` | Root of `.backup.ini`; relative paths are resolved from the process cwd |
| `<source>.path` | Required and non-empty; `.` selects the `.backup.ini` root. Relative paths are resolved from the source root; absolute paths are used as written |
| `targetpath` | If present, destination = `targetRoot + "/" + targetpath`; otherwise the source basename, with a trailing `/` producing an empty basename |
| `delete` | Only case-insensitive `true` enables deletion; yes/on/1 remain false |
| `exclude` | Split on commas and trim each element; empty elements are passed as `--exclude=` |
| `compression` | Defaults to zstd; specified value is passed to Btrfs when creating a new subvolume; command failure stops preparation |

`--config` does not change path resolution bases: target remains relative to `invocationDir`, roots remain relative to cwd. Raw configured path strings are stored separately from canonical paths for safety checks. Data is never silently renamed; an unsafe destination is rejected with a diagnostic.

The rsync archive set is `-a -x`, without automatic `-H`, `-A`, `-X`, or `--delete-excluded`. No `one_file_system` setting is introduced. Unknown INI fields are ignored. Parser behavior includes case-insensitive keys with case-sensitive sections, ordered DEFAULT inheritance, lazy interpolation `%(name)s`/`%%` (only when accessing a section, depth up to 10), multiline values, full-line comments, and rejection of duplicate keys/ordinary sections. A repeated exact `[DEFAULT]` is allowed if it contains no duplicate keys. The parser has no artificial Scanner line limit; configuration files are treated as trusted local data.

The Plan contains TargetRoot, SnapshotRoot, and ordered Jobs with Name, SourcePath, Destination, SnapshotPrefix, Delete, Excludes, Compression, and Origin(File, Section). `internal/pathresolve` resolves existing symlinks before `..`, preserving missing suffixes. A managed `targetpath` containing a `..` component is rejected before cleanup so symlink redirection cannot be hidden; source and root paths use non-strict realpath resolution. A `.backup.ini` that is not a regular file is skipped with a warning. The rsync/Btrfs adapters do not parse INI.

### 3.3. Placement and Daily Rotation

The configured `target.path` and `target.snapshots` define storage placement. The destination is the existing subvolume named by the source basename or `targetpath`; no additional `work/<job-id>` or `snapshots/<job-id>` levels are created.

SnapshotPrefix = basename of destination; if `targetpath` is specified, prefix = the entire `targetpath` with every `/` replaced by `-`. For example, `machine/docs` becomes `machine-docs`. Conflicting destinations and prefixes are detected during planning, without automatic renaming.

The date is determined using the local calendar separately for each job **after successful rsync**, in `YYYY.MM.DD` format. Full snapshot path: `<target.snapshots>/<prefix>-<date>`.

1. If the daily path does not exist, create a read-only snapshot with that name.
2. If it exists, find the first free suffix starting with `.0`, then `.1`, `.2`, and so on; rename the old daily snapshot to that name.
3. Create a new read-only snapshot under the primary daily name.

Example after three successful runs on one day: `documents-2026.11.03` is the latest, `.0` is the first, `.1` is the second. Gaps in suffixes are reused; existing snapshots are never overwritten, renumbered, or deleted. Run IDs/UUIDs and random suffixes are not used in snapshot names.

Rename failure prevents creation of a new snapshot. Snapshot failure after rename leaves the old snapshot under the found suffix; there is no automatic rollback. Before rename, the existing leaf is checked to be an expected read-only Btrfs snapshot; a symlink/ordinary directory/unrelated object is not moved. This is a protective check, not a new rotation algorithm.

Rotation and snapshot creation run under the storage lock. The date is determined separately for each job, so jobs may receive different dates if a run crosses midnight. There is no automatic retention; snapshots remain in place.

## 4. Layers and Ownership

```text
cmd/gobackupper/       composition root, flags, signals, exit mapping
internal/config/      INI loader, typed model
internal/backup/      Plan, Job, policy, application service, Result
internal/rsync/       argv, output decoder, Synchronizer implementation
internal/btrfs/       repository layout, lock, subvolumes, snapshots
internal/process/     sole lifecycle manager for external processes
internal/pathresolve/ realpath without premature normalization
internal/ui/          progress state, TTY/plain renderer
```

We do not introduce a generic FileSystem, a general Mount/Unmount hierarchy, or plugins. Btrfs storage and file synchronization have different guarantees; another backend requires its own contract, not an invented universal interface.

Interfaces are defined in `backup` on the consumer side:

```go
type Synchronizer interface {
    Check(context.Context) error
    Sync(context.Context, Job, func(Progress)) (SyncResult, error)
}

type Repository interface {
    Check(context.Context) error
    Acquire(ctx context.Context) (release func() error, err error)
    Prepare(ctx context.Context, job Job) (Workspace, error)
    Snapshot(ctx context.Context, workspace Workspace, date time.Time) (Snapshot, error)
}
```

The actual types and interfaces are in `internal/backup/types.go`. The process runner is a concrete type. The service receives `func() time.Time` and calls it after Sync; Repository uses the local date and owns suffix selection, rename with RENAME_NOREPLACE, and snapshot creation as one operation. Snapshot contains the final path and the path of the renamed previous snapshot; on partial failure that path is also included in the error text. The Repository interface checks backup policy; it does not promise arbitrary future backends.

`backup.Service` handles transitions, not escape sequences or command construction. `Repository` owns target paths, compression, and locking. `Synchronizer` does not create destinations or delete snapshots. UI does not receive exec.Cmd and does not determine success from percentage.

## 5. Workflow and Errors

1. Load config.ini and all discovered `.backup.ini` files, perform structural validation, and explicitly select jobs (`--all` or repeatable `--job <section-name>`). EOF does not silently start all jobs. validate performs no copying.
2. Build a plan for selected jobs; check executable/capabilities, source and target root existence, and overlaps among destinations and snapshot prefixes. Do not mutate unselected jobs.
3. Acquire exclusive flock `<targetRoot>/.gobackupper.lock`, then `<snapshotRoot>/.gobackupper.lock` for the entire run; create snapshot root if needed with one mkdir under the first lock. The second lock protects a shared snapshot directory among configs with different target roots. If canonical roots are identical, acquire the lock only once. Both locks are nonblocking: if the second is busy, release the first and return an error rather than wait while holding the first. Descriptors are held until children finish; lock files are not deleted.
4. Under locks, recheck storage identity/paths, prepare all selected workspaces and compression. Existing subvolumes do not automatically receive new compression; properties are checked but not changed. Checking selected jobs before rsync detects preparation errors early.
5. Sequentially for each job: Sync → check exit status → local date → rotate old daily snapshot → create new read-only snapshot → JobResult.
6. The first error stops subsequent jobs. Successful earlier results remain in RunResult.
7. Release locks after processes finish; return result and error.

Target root must exist and be on Btrfs. This is not grounds to format, mount, or request sudo automatically. Snapshot root may be inside target root or a separately configured directory; if absent, create it with one mkdir, not mkdir-p. Destination parents must exist. Destination and snapshots must belong to the same Btrfs filesystem; identical mountpoint strings are not a sufficient check.

Path checks: canonical existing parents; prohibit overlap of source with destination or snapshot storage, identical/nested destinations, and overlap of destination with snapshot storage; reject symlinks in managed leaves and unexpected mountpoints in the managed tree. An empty basename/targetpath must not direct copying into the target root itself. An ordinary directory in place of the workspace subvolume is not accepted. Canonical identity of existing target/snapshot roots is allowed, but destination must be a separate safe subvolume with no conflict with snapshot names. Locks do not protect against external rsync/administrators or partially overlapping storage roots using different lock files: such concurrent configurations are unsupported. Path checks are subject to filesystem races; locks coordinate only GoBackUpper instances using the same lock paths.

Rsync != 0, including 23/24: no new snapshot is created and the old daily snapshot is not renamed; workspace may have been partially updated. Rename failure: old daily name remains and no new snapshot is created. Snapshot failure after rename: workspace is current, previous snapshot remains available under its suffix, and the new backup is not confirmed. Compression failure: stop before Sync. Nothing is automatically deleted or rolled back.

A snapshot after rsync atomically captures the workspace, but does not provide an application-consistent state of a changing source. Databases require separate application hooks/dumps; these are not currently part of product guarantees.

An error contains stage, job name, argv (not a shell string), process exit code, and a bounded diagnostic tail. `%w`, errors.Is/As; os.Exit only in main. Proposed CLI codes: 0 success, 2 config/usage, 3 repository/preflight/lock, 4 sync, 5 rotation/snapshot, 130/143 SIGINT/SIGTERM cancellation. Progress reaching 100 has no bearing on this choice.

## 6. Rsync Progress: A Controlled Text Protocol

### 6.1. Launch Mode

Base output arguments:

```text
rsync -ax [--delete] [--exclude=...]
  --info=progress2
  --outbuf=N
  --no-human-readable
  --out-format=GBU|%i|%l|%n
  -- <absolute-source/> <absolute-workspace/>
```

`LC_ALL=C` is set only for the child process. We remove `-v` and do not enable `--progress` alongside progress2. `--out-format` itself enables name output; `name0` must not be used here or file events will be lost. `--no-human-readable` stabilizes the byte counter; speed may still be `kB/s`, `MB/s` and is stored as a display string.

`--outbuf=N` disables stdout buffering; PTY/stdbuf are unnecessary. A PTY mixes terminal semantics with the protocol and complicates cancellation and stream handling. However, rsync publishes progress at its own cadence: unbuffered does not mean an event for every block.

The minimum CLI functionality requires rsync options from the 3.1+ family. Use a security-patched rsync supported by the distribution. Check versions/capabilities once before launch; do not rely only on ad hoc regular expressions. Do not claim compatibility with all versions without a fixture/smoke matrix.

### 6.2. What Can Be Shown Reliably

| Field | Meaning | Limitation |
|---|---|---|
| reported bytes / percent | Numbers published by progress2 | Not physical bytes written and not an overall backup percentage |
| rate | rsync speed string | Reconstructed data rate; not necessarily network/disk speed |
| `ir-chk` | Incremental file discovery is continuing | Denominator is still changing; percentage may decrease |
| `to-chk=remaining/total` | Checking the item list | Items include more than transferred regular files; this is not a byte percentage |
| `xfr#N` | Regular-file transfer count | Not the number of successfully saved snapshots |
| file event | Updated item and itemized change code | An event before transfer does not prove successful transfer |
| elapsed | Our monotonic clock from process start | Independent of rsync line format |

Until discovery completion is signaled, state is totalKnown=false. While ir-chk is present, UI shows “scanning and synchronizing” and an estimate, not an exact completion gauge. Short lines without suffix update bytes/rate but do not reset a previously known mode. `to-chk` allows showing a stable list size, but does not provide a guaranteed finish time.

Rsync's time field has different meanings: an intermediate line shows ETA, a completed line shows elapsed. Do not interpret every `0:00:02` as “2 seconds remaining.” Without confident classification, show only our own elapsed time and rate.

A fast no-op run may have no useful progress; this is normal. 100% transferred does not mean metadata, deletion, process, or snapshot work is complete. UI has separate Preparing, Syncing, Snapshotting, Succeeded, Failed, Cancelled states; do not invent a “90% rsync + 10% snapshot” distribution.

**Do not run a preliminary dry-run/WalkDir for an exact percentage:** this doubles traversal, adds I/O, and races with a changing source; filters/mount boundaries would need to be duplicated. `--no-inc-recursive` provides a complete file list in advance, but increases startup latency and memory—it is not the default and does not promise an exact ETA.

### 6.3. Framer → Decoder → State

**Framer:** accepts arbitrary stdout byte chunks; extracts records on CR or LF, treating CRLF as one delimiter. Handles delimiters at chunk boundaries and an unterminated final record at EOF. Maximum buffered record size is 64 KiB; after overflow, skips until the next delimiter, marks telemetry loss, and continues draining. Does not hang on Scanner limits or accumulate all stdout.

**Decoder:** first a file record with fixed prefix `GBU|`, then progress grammar, otherwise diagnostic record. A file record is split at the first three `|` characters: prefix, itemize, decimal length, and all remaining text is the name. Names may contain `|`, spaces, and tabs; do not apply strings.Fields to the whole record. Keep %n last; do not add %L. Do not use `%b`/`%c`: transfer-statistic fields shift file output until transfer completion, whereas we need an event before transfer. Current file changes only for an appropriate transfer item, not every metadata/deletion line.

Progress accepts leading spaces, integer bytes (with separators if needed for fixtures from other versions), percentage, rate, time, and optional `(xfr#..., ir-chk=.../...)`/to-chk suffix. Short intermediate lines are normal; requiring “exactly six fields” is incorrect. Numeric overflow or unexpected grammar must not affect filesystem decisions.

**Escaping:** rsync uses `\#ooo` for unsafe bytes; newline/ESC are escaped, tabs may remain. Decode escapes once into bytes, then safely display UTF-8. Repeated decoding breaks literal `\#012`: rsync outputs it as `\#134#012`. After decoding, re-escape controls for the UI; never print decoded ESC/newline as terminal control commands. Filename from stdout is for display only—never use it for filesystem operations.

Unknown records are retained in a bounded diagnostic tail and do not become the “current file” by default. If all progress is incompatible with a newer version, backup continues based on exit status with a “progress unavailable” message. This degrades observation, not copy error reporting.

**State:** one decoder writer updates a small structure under a mutex; renderer receives a snapshot of the structure 5 times per second. No shared unsynchronized maps or goroutine per file. Old intermediate samples are overwritten, not queued. Heavy UI is not called from the reader: the adapter callback only updates state and never writes to the terminal. Final Result and phase transitions are preserved independently of progress samples.

### 6.4. Process Lifetime

The process runner has separate bounded stdout decoder and stderr tail writers. `exec.Cmd.Stdout/Stderr` receive these writers; os/exec reads streams concurrently and Wait waits for copying to finish. Do not use CombinedOutput or mix stderr with telemetry. Writers do not block on UI or disk logger and do not return errors for unknown records.

After Wait, the decoder processes the remaining record at EOF. Stream errors and exit status are returned to the caller; unknown lines do not become copy errors. Diagnostic tail is 64 KiB per stream, with truncation marked. UI receives only the latest state and rechecks phase before writing a deferred frame.

Child runs in a separate Linux process group. Runner observes leader completion through Waitid(WNOWAIT), without releasing PID/PGID before the final signal to the group. Cancellation sends SIGTERM, then after a grace period SIGKILL to the group, including descendants that outlive the leader. Then cmd.Wait drains streams and reaps the leader. On normal leader completion, any remaining descendants are also killed before reap. WaitDelay bounds inherited pipes, not the duration of a normal backup. This ordering prevents signaling a reused PID and leaking a TERM-ignoring descendant. UI cancels context; it does not control the process handle.

## 7. Validation of the Selected Approach

The progress protocol was tested with installed `rsync 3.5.1`, ordinary stdout/stderr pipes, `LC_ALL=C`, the selected out-format, and output flags. Copying was limited with `--bwlimit=1024` only to observe live updates. Source/destination were created in a temporary directory and removed after the check; user data was not touched.

Test set: one random 3 MiB file and six small files with spaces, tab, newline, ESC, Cyrillic, and literal `\#012` in their names. Reading chunks of 113 bytes checked independence from read boundaries.

Results with assertions:

- exit code 0, stderr empty;
- intermediate progress records received before process completion, including around 1.14 and 2.14 seconds;
- short lines without xfr/check suffix and final lines with suffix received;
- all seven files verified by comparing source/destination SHA-256;
- newline and ESC emitted as octal escapes, tab remained literal;
- literal `\#012` emitted as `\#134#012`;
- --no-human-readable leaves rate with units, but bytes in the tested version had no separators.

The Go implementation passed `go test ./...`, `go test -race ./...`, `go vet ./...`, and an end-to-end run of the built CLI with real rsync, TempDir configs, and a controlled CLI model of Btrfs. Delete false/true, excludes, symlink, content versions, and daily .0/.1 were checked; in a real PTY, live progress and SIGTERM 143 without rename/snapshot were checked.

Permanent behavioral tests cover parser/config/order, unsafe plans, phases and partial successes, large streams and cancellation descendants, framing/escapes/overflow, locks, compression failures, and rotation. Existing snapshots are not overwritten, failed sync does not initiate rotation, and failed snapshot reports the retained previous path.

**Limitation:** the Btrfs model does not confirm kernel snapshot behavior, actual read-only status, or compression. The real Btrfs CLI was checked to fail on a temporary target on tmpfs without mutations. A dedicated accessible Btrfs test filesystem was not provided; formatting, mounting, root, and user media were not used. Integration acceptance on such a filesystem must verify read-only properties and snapshot contents. Other rsync versions and `-x` on a separate test mount were not checked. Detailed matrix: docs/development.md.

## 8. References and Decision Summary

- [Official rsync manpage](https://download.samba.org/pub/rsync/rsync.1): outbuf, out-format, progress/progress2, escaping, incremental recursion.
- [gokrazy/rsync README](https://github.com/gokrazy/rsync): supported scenarios and its own maturity warning.
- [rsynccmd public API](https://github.com/gokrazy/rsync/blob/main/rsynccmd/rsynccmd.go): context, streams, final stats.
- [grsync Task implementation](https://github.com/amilstead/grsync/blob/HEAD/task.go): reviewed progress/state/logging limitations.
- [go-ini/ini](https://github.com/go-ini/ini): reviewed but unselected candidate; actual INI loader uses stdlib.
- [golang/term](https://github.com/golang/term): terminal support.

The standard library and a small number of targeted packages keep the implementation focused on backup policy and reliable integration. The most important boundary: **progress is optional observation; a successful backup is a verified sync result plus a created read-only snapshot.**
