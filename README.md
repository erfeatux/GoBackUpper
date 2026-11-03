# GoBackUpper

Local backups powered by system `rsync` and read-only Btrfs snapshots. Runs on Linux; requires Go 1.23+ to build.

GoBackUpper is a standalone Go application for synchronizing selected sources into Btrfs subvolumes and creating dated, read-only snapshots after successful runs.

## Features

- Run selected backup jobs explicitly, one after another.
- Use `rsync -ax`, with optional deletion and rsync exclude patterns.
- Create Btrfs work subvolumes and configure compression when they are first created.
- Keep daily snapshot history without automatically deleting older snapshots.
- Validate configuration, paths, dependencies, and storage before a run.

GoBackUpper does not provide scheduling, cloud storage, mounting or formatting, automatic privilege escalation, retention, compressed archives, or encryption. Use an external systemd timer or cron for scheduling, and prevent concurrent external writers.

## Requirements

- Linux; Go 1.23+ to build (not required to run a built binary).
- `rsync` 3.1+ and `btrfs-progs` available in `PATH`.
- An existing target directory on Btrfs. Snapshot storage must be on the same Btrfs filesystem, though it may use a different mount point.
- Permissions to read sources, write the target, create subvolumes and create read-only snapshots. GoBackUpper does not elevate privileges.

The `-a` option does not include ACLs, extended attributes, or hard links (`-A/-X/-H`). The `-x` option does not cross filesystem boundaries. A snapshot of a work directory does not guarantee consistency for a changing database in the source.

## Build

```sh
mkdir -p bin
go build -o bin/gobackupper ./cmd/gobackupper
bin/gobackupper help
```

## Quick start

1. Choose an **existing** Btrfs target; do not use the only copy of valuable data as the destination.
2. Put `config.ini` next to the invocation path (the symlink directory if applicable) or pass `--config`. Start with the [main INI example](docs/examples/config.ini) and [source INI example](docs/examples/.backup.ini).
3. Put a `.backup.ini` in each root declared in `[sources]`.
4. Validate, then run selected jobs:

```sh
bin/gobackupper validate --config bin/config.ini
bin/gobackupper run --config bin/config.ini --job Documents
```

`run` without a selection does not copy anything. For path resolution, configuration details, command behavior, safety notes, and recovery steps, see the documentation below.

## Documentation

- [Configuration and paths](docs/configuration.md)
- [Running jobs, progress, errors, and cancellation](docs/usage.md)
- [Recovery and partial failures](docs/recovery.md)
- [Development and verification](docs/development.md)
- [Architecture and design decisions](DESIGN.md)

## License

GoBackUpper is distributed under the [GNU General Public License v3.0](LICENSE).
