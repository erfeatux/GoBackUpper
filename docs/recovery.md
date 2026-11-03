# Recovery and partial failures

## Choosing a snapshot

The latest snapshot for a day is named `<prefix>-YYYY.MM.DD`. `.0` is the first moved daily snapshot, `.1` the next; suffixes may have gaps. The number is not a global version. If creating a new snapshot fails after rename, the primary name may be absent and the most recent prior snapshot will remain under a suffix.

Check that the chosen path is read-only:

```sh
btrfs subvolume show /backup/Snapshots/documents-2026.11.03
btrfs property get /backup/Snapshots/documents-2026.11.03 ro
```

Expect `ro=true`. Do not change it to false for ordinary recovery. A snapshot is an independent Btrfs subvolume with shared extents, not a complete independent archive on another device: filesystem loss can destroy the entire history.

## Recovering to a separate directory

First choose an existing **empty test directory** with sufficient permissions and space. The following command does not delete extra files in the destination:

```sh
rsync -a -- /backup/Snapshots/documents-2026.11.03/ /restore/documents/
```

The trailing `/` on the source means to copy its contents. Check names, contents, and metadata before replacing working data. If a writable Btrfs clone is needed, an administrator can separately create a writable snapshot in a new, unused location; the application does not do this automatically.

Do not use `--delete` for a trial recovery, and do not restore over the only working copy. The application's `run` command is **not a restore**: data always flows from source to workspace, and it can destroy workspace changes when `delete=true`.

The backup profile does not additionally preserve ACLs, xattrs, or hard-link topology. For these requirements, enabling flags only during restore is insufficient; the metadata must have been preserved during backup. UID/GID and special files require appropriate permissions; the application does not turn an unprivileged copy into a complete system image.

## Sync failure

For any nonzero rsync result, including 23/24:

- No new snapshot is created.
- The previous daily name is not renamed.
- The workspace may contain a mixture of new and old data.
- Previous successful snapshots remain available for recovery.

Fix the source, permissions, or excludes, then run again. Do not consider the workspace a confirmed backup or delete history to free space before checking that the needed data is present.

## Rotation or snapshot failure

If rename fails, creation of a new snapshot does not begin. An existing daily name is not overwritten.

If rename succeeds but `btrfs subvolume snapshot -r` returns an error, the previous snapshot is available at the path shown as `previous snapshot retained at ...` in diagnostics. The primary daily name may be absent. The application does not roll back: it does not try to rename the snapshot back or delete a partially created object.

Steps to take:

1. Wait for the process to finish and release its locks.
2. Inspect both paths with `btrfs subvolume show` and `property get ... ro`.
3. Check the contents and cause of the error (space, permissions, read-only filesystem, or a different filesystem).
4. Fix the cause, then repeat `validate` and `run`.

If cancellation occurs during snapshot creation, the external command may have created a snapshot before receiving the signal. Do not infer that the object is absent just from the cancellation exit code.

## Compression failure

Creating a work subvolume and setting compression are separate operations. If the latter fails, the subvolume remains so that unknown data is not automatically deleted. On the next run, an existing workspace is checked as a writable subvolume, but its compression is not reconfigured.

After determining the cause, set its property manually or choose another unused `targetpath`. Deleting a subvolume is a separate, potentially destructive administrative operation, not part of the application's automatic cleanup.

## Reliability limitations

- Copying a source tree is not application-consistent. For a running database, use an appropriate application dump/stop workflow before backup.
- Nested Btrfs subvolumes do not automatically become recursive snapshots.
- A read-only snapshot protects against ordinary file changes, not an administrator, disk damage, or a malicious process with sufficient privileges.
- Disaster recovery requires a separate copy on another device or node and regular recovery testing. This application does not implement replication and is not a substitute for that strategy.
