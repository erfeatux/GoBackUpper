# Configuration

## Two levels

By default, `config.ini` is located next to the path used to launch the executable. When launched through a symlink, the symlink's directory is used, not the target binary's directory. `--config`/`-c` selects a different main file; configured target paths still use the invocation directory as their base.

```ini
[target]
path=../backup-target
snapshots=../backup-target/Snapshots

[sources]
user=/home/alice
shared=/srv/shared
```

In `/home/alice/.backup.ini`:

```ini
[targets]
docs=Documents
photos=Photos

[Documents]
path=Documents
targetpath=alice-documents
delete=true
exclude=*.tmp, cache/, .backup.ini
compression=zstd

[Photos]
path=Pictures
delete=false
```

This defines the `Documents` and `Photos` jobs, in declaration order. The `docs` and `photos` keys are references, not job names. `/srv/shared/.backup.ini` is loaded after the first file. A missing `.backup.ini` produces a warning and is skipped; having no jobs at all is an error. A malformed existing configuration is an error, not silently skipped.

Unknown fields are ignored.

## Paths: read carefully

| Value | Base and result |
|---|---|
| `target.path`, `target.snapshots` | `<invocation-directory>/<value>`, then realpath; **not** a normal absolute-path override |
| `[sources].<key>` | Absolute paths are used as-is; relative paths are based on the launch working directory (cwd) |
| `<source>.path` | Relative paths are based on the root containing `.backup.ini`; absolute paths are ordinary absolute source paths |
| `targetpath` | Appended to the target root; it is not an independent absolute path |
| No `targetpath` | Destination is target root plus the source basename |

For example, with invocation path `/opt/gobackupper/bin/gobackupper` and `target.path=../backup-target`, the result is `/opt/gobackupper/backup-target`. But `target.path=/mnt/backup` resolves to `/opt/gobackupper/bin/mnt/backup`, **not** `/mnt/backup`.

Target paths are resolved relative to the invocation directory using the rules in the table above. For example, use `../../../mnt/backup` to refer to `/mnt/backup` from invocation path `/opt/gobackupper/bin/gobackupper`. Always inspect the paths printed by `validate` before running.

`~` and `$HOME` are not expanded. Do not change cwd between validation and execution if `[sources]` contains relative paths. An absolute source ending in `/` with no `targetpath` has an empty basename, so the resulting destination is rejected. Specify a safe `targetpath` instead of relying on Go path normalization.

The parent of `target.path` is not created automatically: the target itself must already exist. If the snapshots directory is absent, one directory is created, not a chain of directories. The parent of a nested `targetpath` must exist. The destination must be a separate Btrfs subvolume; an ordinary directory with the same name is neither converted into a subvolume nor silently accepted.

## Source fields

| Field | Requirement and behavior |
|---|---|
| `path` | Required; the source directory is checked for the selected job |
| `targetpath` | Optional; destination is relative to the target root. Empty or unsafe values are rejected by the planner |
| `delete` | Only `true`, case-insensitively, enables `--delete`; missing/false/yes/on/1 disables it |
| `exclude` | Optional; split on commas and trim each pattern. Empty elements are passed to rsync as `--exclude=` |
| `compression` | Optional; defaults to zstd. Passed to `btrfs property set` only when creating a new subvolume; an explicitly empty value does not get replaced by the default |

Excludes use **rsync** syntax, not Go `filepath.Match`. Commas are separators; CSV escaping is not supported. `--delete` does not enable `--delete-excluded`, so excluded files are generally protected from deletion. Pass only the patterns you need and inspect the work copy. `.backup.ini` itself may be included in the backup if `path` points to the directory containing it and the file is not excluded.

`-a -x` are fixed: source contents are copied, symlinks are not dereferenced, and other filesystem boundaries are not crossed. ACLs, xattrs, and hard links are not additionally enabled. Do not add arbitrary rsync flags to INI: unknown keys are not a way to bypass the application's contract.

## Ordering, DEFAULT, and interpolation

Keys are case-insensitive; section names are case-sensitive. `[Documents]` and `[documents]` are different sections and `--job` names. Root and target order is preserved. If different `.backup.ini` files define the same source name, the last definition replaces the earlier one while retaining the position of its first occurrence.

Supported features include `[DEFAULT]` inheritance, `%(key)s`, escaped percent signs `%%`, indented multiline values, and full-line `;`/`#` comments. Inline `;` and `#` are part of the value; put a comment on a separate line.

DEFAULT keys are inherited by **all** sections, including `[sources]` and `[targets]`. This may unexpectedly add references: do not put arbitrary shared parameters in DEFAULT without understanding this behavior. Repeating a key with the same name ignoring case in one section is an error. Repeating a normal section is an error. Interpolation errors and cycles are reported with context and do not cause infinite recursion.

## Snapshot names

Without a `targetpath` prefix, the snapshot prefix is the destination basename. With a `targetpath` prefix, the entire value is used with `/` replaced by `-`: `alice/documents` becomes `alice-documents`. The snapshots directory is shared, with no additional job-ID level.

The local date is determined **after successful rsync**. Three runs in one day produce:

```text
Snapshots/
  alice-documents-2026.11.03       latest
  alice-documents-2026.11.03.0     first
  alice-documents-2026.11.03.1     second
```

The first available suffix is used, including gaps. No snapshots are automatically deleted. `targetpath` values `a/b` and `a-b` collide on the prefix; the planner rejects the conflict rather than renaming either.

## Path and safety restrictions

The planner rejects unsafe paths, including copying over the target root, any `..` component in `targetpath`, overlapping destinations, a source inside its destination, symlinks in managed leaves, and conflicting snapshot prefixes. Source and target roots are resolved by following existing symlinks before processing `..`; the parent-component restriction applies to managed `targetpath`. Source availability is checked only for selected jobs; unselected destinations are not prepared. Compression and process errors stop the run. The CLI requires an explicit job selection.
