# Keeping the database local in iCloud Drive

For size diagnostics and explicit compaction, see [database maintenance](database-maintenance.md).

Noema normally stores its SQLite database in `db/` inside each cortex. Database
writes, including background federation bookkeeping, can cause frequent iCloud
uploads even when no Markdown traces change. Opt-in `nosync` storage moves the
whole database directory to `db.nosync/`. Traces remain in their usual folders.

## Enable or disable

Stop every Noema server, watcher, and agent connection using this cortex before
changing storage. Upgrade all clients that will open it. New clients enforce a
local process lock outside the cortex, under `noema/storage-locks/` in
`XDG_RUNTIME_DIR` (or the operating system's temporary directory). Keep that
runtime directory local and consistent across clients. The lock uses the
canonical cortex path, so path aliases share the same lock. Replacing the
compatibility lock inside a synced cortex cannot bypass this runtime lock.

Clients also retain the previous cortex-local lock for upgrade compatibility.
Restart existing clients to gain the runtime-lock protection; versions predating
storage locking must be stopped manually. Locks do not coordinate different
computers over iCloud.

This is an offline maintenance operation. Stop direct SQLite tools and older
clients too, and prevent automatic restarts until migration finishes. A successful
initial checkpoint does not reserve the database against a new, nonparticipating
SQLite writer. Noema's process lock coordinates compatible Noema clients; it
cannot guarantee safe directory migration while another program ignores that
lock. The required backup captures the state before migration, not subsequent
writes by an unsupported concurrent client.

Choose a new backup filename outside the cortex, preferably outside iCloud Drive:

```sh
noema cortex storage mycortex
noema cortex storage mycortex --database nosync --backup /path/to/backups/before-nosync.tar.gz
```

The command checks database integrity, checkpoints the WAL, writes a complete
backup, and moves the database directory. It preserves event history, federation
state, embeddings, pending recovery records, and files in the database directory.
It refuses conflicting database directories and never overwrites an existing
backup. Restart your Noema clients after it succeeds.

To return to the original layout, stop clients and run:

```sh
noema cortex storage mycortex --database default --backup /path/to/backups/before-default.tar.gz
```

Reversing the setting makes the database eligible for iCloud syncing again.
Repeating an already completed setting is a no-op. A previously renamed
`db.nosync/` directory can be adopted using the same enable command and backup.

## Configuration and compatibility

The command maintains `storage.yaml` in the cortex root:

```yaml
database: nosync # default or nosync
```

Use the command to change an existing cortex; editing this field alone does not
move the database. Normal cortexes without this file continue using `db/`.
The command preserves other YAML keys, comments, and their order; the `database`
entry must use a top-level `database:` line for command-based editing.
On Unix, existing `storage.yaml` permissions are preserved, including when
resuming an interrupted migration; a new file defaults to `0640`.
It leaves `cortex.md` and the global registry untouched.

With `nosync` enabled, `db/noema.db` is a small, static compatibility guard, not a
second database. Older Noema versions fail to open it instead of creating an
empty database. Do not delete or replace this guard. Use a compatible version
to reverse the migration before downgrading. Do not rename the WAL or SHM files
individually, or replace the database directory with a symlink.

## Interrupted migrations and backups

An interrupted migration blocks normal database access. After stopping clients,
finish it with:

```sh
noema cortex storage mycortex --resume
```

Resume completes the original direction. To undo it, finish the interrupted
migration and then run the command for the opposite storage mode. The original
backup is also available through Noema's ordinary cortex restore workflow.
Do not delete the migration journal to bypass recovery.

`noema cortex backup` includes `db.nosync`, its recovery files, the configuration,
and the compatibility guard. Restore preserves this layout. Keep complete Noema
backups: Markdown alone does not preserve the database's event and federation
state. Historical event records may name a recovery artifact under the previous
directory prefix; the artifact moves with the database directory.

## iCloud scope

Apple documents `.nosync` as an exclusion from iCloud transfer. It also documents
that deleting or evicting a parent directory removes its `.nosync` children.
Keep the cortex's parent folder downloaded and maintain backups outside iCloud.
See [Apple's iCloud storage documentation](https://developer.apple.com/library/archive/documentation/General/Conceptual/iCloudDesignGuide/Chapters/iCloudFundametals.html).

This setting does not disable Markdown syncing, change SQLite's WAL behavior,
or promise exclusion from Dropbox, OneDrive, or other providers. The database
can continue changing locally while iCloud ignores its directory.

On another device, iCloud can deliver the setting and guard without the excluded
database. Noema reports the missing local database rather than silently creating
one. Restore a complete Noema backup to establish that device's local copy;
do not treat iCloud as replication for the database or run copied cortex
identities as independent federation peers.
