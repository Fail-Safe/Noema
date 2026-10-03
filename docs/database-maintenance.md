# Database maintenance

Noema exposes storage diagnostics and explicit compaction for both `db/` and
`db.nosync/`. It does not schedule full vacuuming or change SQLite's auto-vacuum
setting automatically.

## Inspect storage

```sh
noema cortex storage mycortex
noema cortex storage mycortex --json
```

Diagnostics can run while Noema clients are active. They read database statistics
without running cortex startup, rebuilding search, pruning records, or requesting
a WAL checkpoint. A missing database is reported rather than created.

The report includes:

- The selected layout and database path.
- Physical database file size and logical size, including pages represented in WAL.
- Completely free pages, their byte size, and their percentage of the database.
- WAL file size and the current auto-vacuum mode.
- Available space on the database filesystem and conservative compaction headroom.

Free pages remain available for future database writes. Their total is not an exact
prediction of compaction savings: a full vacuum can also repack partially filled
pages. A large WAL is a separate issue from free space in the main database.
Statistics are a snapshot; file sizes can change while other clients write.
JSON size fields use bytes and `reusable_percent` uses the range 0–100.

## Compact explicitly

Stop all servers, watchers, and agent connections using the cortex, then choose
a new backup archive outside its directory:

```sh
noema cortex compact mycortex --backup /path/to/backups/before-compact.tar.gz
```

Add `--json` for a structured result containing `before`, `after`, `backup_path`,
and `reclaimed_bytes`. The ordinary output reports before/after file sizes, free
space, the final WAL size, and the backup location. `reclaimed_bytes` measures
reduction of the main database file, not changes in the WAL.

The command:

1. Acquires the exclusive Noema storage lock and opens the existing database.
2. Checks database integrity and available space.
3. Checkpoints and closes SQLite while retaining the Noema maintenance lock, then
   creates a complete cortex backup. Existing backup files
   are never overwritten, and backup paths inside the cortex are refused.
4. Reopens SQLite, rechecks space after creating the backup, then runs `VACUUM`.
5. Checks integrity again, checkpoints/truncates the WAL, and reports the result.

Supported Noema clients prevent compaction while their database connection is
open. Older clients and direct SQLite tools must also be stopped; they do not
participate in Noema's process lock. Compaction does not stop or restart services
for you. Choose a maintenance window and restart clients after success.

The space check conservatively requires twice the larger of the logical database
size and the main file size to be available on the database filesystem, in addition
to the space used by the backup. SQLite may also use temporary storage; ensure
that filesystem has room if configured separately. Space checks cannot reserve
capacity against other applications writing concurrently.

Compaction preserves retained traces, event history, embeddings, federation state,
and the selected storage layout. It does not implement retention or remove old
events, and it does not rewrite Markdown files or configuration.

## Interrupted or unsuccessful compaction

Noema uses SQLite's transactional `VACUUM`; it does not replace the database file
with a separately generated copy. SQLite's normal journal/WAL recovery applies
after a process interruption. Leave the database and its sidecars together.

The complete backup is written before vacuuming begins. If a later step fails,
the error identifies the retained backup path. After stopping competing clients
or resolving disk-space errors, verify the cortex and retry using a new backup
filename. There is no compaction-specific `--resume` journal. The existing
`noema cortex restore` workflow can restore the backup if recovery is needed.

A full vacuum rewrites the database and can generate substantial I/O. Consider it
after large deletions or when diagnostics show enough unused space to justify a
maintenance window. A fixed calendar interval is not required for normal use.
See [SQLite's VACUUM documentation](https://www.sqlite.org/lang_vacuum.html).
