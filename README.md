# checksumtool

checksumtool is a command-line utility for calculating and comparing file checksums. It allows you to efficiently manage checksums for a large number of files and detect any changes or mismatches.

My personal use case is to detect bit rot in any pictures. Throughout the years old photos get corrupted. This utility detects corrupted photos, allowing me to restore them from backups.

## Features

- Calculate checksums for files in one or more directories
- Compare checksums against a stored database to detect changes or mismatches
- Tell suspected corruption apart from ordinary edits, using the recorded size and timestamp
- Detect moved files and relocate their database entry instead of reporting a deletion plus an addition
- Scan once, review, then apply without re-reading a single file
- Optional schema-versioned observation manifests for machine consumers
- Quick daily syncs that read only new and changed files, with rolling verification that re-reads a fixed amount of unchanged data per run so every file is still checked regularly
- Apply only chosen categories (for example additions and moves) and write per-category path lists for other tools
- Update the checksum database with new or changed files
- List files that are missing from the checksum database
- Add checksums for missing files to the database
- List and remove deleted files from the database
- Optional TOML config file for default directories, workers, and verbose settings
- Progress tracking and estimation of remaining time
- Interrupt handling to save work done so far
- Non-zero exit code on checksum mismatches in check and sync modes, and on changed or deleted files in sync

## Usage
checksumtool [flags] [directories...]

`checksumtool -help` prints the full mode reference, including how each kind of mismatch is classified.

### Flags
- `-db string`: Checksum database file location (default `$HOME/.local/share/checksumtool/checksums.json`)
- `-config string`: Config file location (default `$HOME/.config/checksumtool/config.toml`)
- `-verbose`: Enable verbose output. Suppressed when stdout is not a terminal, so an unattended run does not mail its progress chatter
- `-mode string`: Operation mode: sync, status, apply, apply-observations, recheck, verify-manifest, check, update, list-missing, add-missing, remove-deleted, list-deleted, migrate
- `-workers int`: Number of worker goroutines (default 4)
- `-scan-report string`: Scan report location (default: alongside the database, as `<db>.scan.json`)
- `-manifest string`: With `sync` or `recheck`, write a versioned observation manifest; with `apply-observations` or `verify-manifest`, read one.
- `-select string`: With `apply-observations`, comma-separated observation identities to consider. No unselected finding is applied.
- `-accept-suspect`: With `apply-observations`, explicitly accept selected modified, corrupt or unverifiable content after its bytes are re-read and matched.
- `-apply`: With `sync`, write the scan results to the database in the same run
- `-with-deleted`: When applying, also remove entries whose file is gone
- `-with-corrupt`: When updating or applying, also accept content changes that could not be told apart from corruption
- `-list-all`: With `sync`, also list moved and new files, not just the findings that need attention
- `-strict-moves`: Only treat a checksum group as a move when exactly one file vanished and one appeared
- `-force`: With `apply`, use the scan report even if it is partial or the database changed since the scan
- `-quick`: With `sync`, read only new files, entries whose file is gone and entries whose size or timestamp changed. Unchanged files are not read, so a quick run alone cannot detect bit rot
- `-verify-budget size`: With `sync -quick`, also re-read the unchanged files verified longest ago, up to this many bytes (`K`, `M`, `G`, `T`, 1024-based). Also settable as `verify_budget` in the config
- `-apply-categories list`: With `sync -apply` or `apply`, apply only these categories: `metadata`, `modified`, `moved`, `new`, `corrupt`, `unverifiable`, `deleted`. Corrupt, unverifiable and deleted still also need `-with-corrupt` / `-with-deleted`
- `-list-paths cat=file[,cat=file...]`: With `sync`, write each category's paths to a file, one per line. Categories: `new`, `moved` (new locations), `move_source` (old locations), `modified`, `corrupt`, `unverifiable`, `deleted`, `metadata`. A path containing a line break is not listed and is reported as an error
- `-relative-to dir`: With `-list-paths`, write paths relative to this directory

### Operation Modes
- `sync`: One pass over everything. Verifies database entries, walks the directories for files not yet recorded, and classifies each file as verified, modified, corrupt, moved, new or deleted. Lists only the findings that need a decision — changed, corrupt, unverifiable and deleted files — while moves and additions are summarized as counts unless `-list-all` is given. Reports only and writes a scan report; add `-apply` to write the results in the same run. Requires at least one directory (from arguments or the config file). Exits with code 1 if any concerning finding is present or a file could not be read.
- `apply`: Apply a scan report written by a previous `sync` run, without re-reading any file contents. Applies moved, new, modified and metadata-only changes; deletions and corruption suspects need `-with-deleted` / `-with-corrupt`.
- `apply-observations`: Selectively apply identities from a versioned manifest. Per-entry baseline comparisons let unrelated database changes coexist; selected bytes are re-read and matched before writing. Results are emitted per item as JSON. Suspect content requires `-accept-suspect`; absence requires `-with-deleted` and complete root coverage.
- `recheck`: Re-hash only the supplied paths and write their current classifications to a partial, targeted manifest. Pair with `apply-observations` to reconsider a restored file without rescanning its tree. A targeted check cannot authorize deletion.
- `verify-manifest`: Verify selected observations against an alternate copy of one source root. Reads the copy and manifest only; it does not write the checksum database.
- `check`: Compare checksums of files against the stored database and report any mismatches. Exits with code 1 if any mismatches are found. Directories are optional; if omitted, all DB entries are checked.
- `update`: Update the checksum database with new or changed files. Refuses to overwrite an entry whose mismatch looks like corruption unless `-with-corrupt` is passed. Directories are optional.
- `list-missing`: List files that are missing from the checksum database. Requires at least one directory.
- `add-missing`: Add checksums for missing files to the database. Requires at least one directory.
- `remove-deleted`: Remove files from the database that no longer exist on disk. Directories are optional.
- `list-deleted`: List files in the database that no longer exist on disk. Directories are optional.
- `status`: Summarize the database: entry count, recorded size, oldest and median time since files were last verified, and any unapplied scan report. Read-only.
- `migrate`: One-time conversion of a legacy database to the current entry format. See [Database format](#database-format).

### Telling corruption apart from an edit

Every entry records the file's size and modification time alongside its checksum, so a mismatch can be classified rather than just reported:

- **corrupt** — content changed but size and timestamp did not. No ordinary write does that, so the bytes changed underneath the filesystem. Never applied without `-with-corrupt`.
- **unverifiable** — content changed and there is no recorded size or timestamp to compare against, so an edit cannot be told apart from corruption. Also needs `-with-corrupt`.
- **modified** — content and metadata both changed: an ordinary edit.

Size and timestamp are never used to skip hashing. Bit rot does not touch either one, so skipping a read because the metadata looks unchanged would defeat the purpose of the tool.

### Moved files

A move is not a distinct filesystem event: it is a deletion plus an addition that share content. `sync` is the only mode that sees both halves, because it walks the directories and checks the database in the same pass, and it matches vanished entries against newly discovered files by checksum.

A checksum group is only treated as a move when both sides have equal counts. Duplicate content is normal in a media archive, and when the counts are equal every possible pairing produces the same database — only the printed pairing is arbitrary. Unequal counts are undecidable, so those files are reported as ordinary deletions and additions instead. `-strict-moves` narrows matching to exactly one vanished file and one new file.

A move can never mask corruption, because matching requires the content hash to be intact. A file that was both moved and corrupted simply fails to match and is reported as a deletion plus a new file.

### What sync reports

A growing archive produces moves and additions constantly, and listing every one
of them buries the handful of findings that actually need a decision. So `sync`
splits its findings in two:

**Concerning — listed individually, and each one makes the run exit 1:**

- `modified` — content and metadata both changed. An ordinary edit, but in an
  archive of finished files an edit is still something you want to know about.
- `corrupt` — content changed while size and timestamp did not.
- `unverifiable` — content changed with no recorded metadata to judge it by.
- `deleted` — recorded in the database, no longer on disk.
- read errors — a file that could not be opened or hashed.

**Ordinary — counted in the summary, listed only with `-list-all`, never a failure:**

- `verified` — content matches the database.
- `metadata` — content matches, but size or timestamp drifted and gets refreshed.
- `moved` — a vanished entry and a new file that share content.
- `new` — a file on disk that is not in the database yet.
- `ambiguous` — a checksum group whose two sides had unequal counts, so it is
  reported as deletions plus additions instead of moves. The deletion half is
  concerning; the addition half is not.

When there is something to report, the run ends with a one-line verdict:
`Needs attention: N changed, N corrupt, N unverifiable, N deleted.` This makes
`sync` usable unattended — a non-zero exit means something needs your eyes, not
that you added photos.

**When there is nothing to report and stdout is not a terminal, `sync` prints
nothing at all** — no summary, no verdict, not even a blank line. Cron mails
whatever a job writes, and mail that arrives every month regardless of the
result is mail that stops being read, so the arrival of mail is itself the
signal. Run it on a terminal and the summary and verdict come back.

`-list-all` is deliberately separate from `-verbose`: wanting a progress bar is
not the same as wanting every new file enumerated. `-list-all` also restores the
summary on an otherwise silent run.

### Scan, review, apply

`check` followed by `update` reads every byte twice. `sync` records what it found in a scan report so the decision can be applied without a second read:

```
checksumtool -mode sync            # full read; classifies everything, writes the report
                                   # review the output
checksumtool -mode apply           # stat-only; writes the database
```

`apply` re-stats each file and only writes the recorded checksum if the size and timestamp are unchanged since the scan; anything that changed underneath is skipped and reported so it can be re-scanned. The report also records which database it was computed against, and `apply` refuses a report whose database has since changed, or one left behind by an interrupted scan, unless `-force` is passed.

Deletions are never applied unless `-with-deleted` is given: an unmounted volume makes every file under it look deleted. Files that are readable again are skipped even then. For unattended use, `sync -apply` does both phases in one run under the same rules.

### Notes

- Directory arguments are resolved through symlinks before scanning, so passing a symlinked directory walks the target directory.
- Symlinked files are checksummed through their targets. Symlinked directories found inside a scan are skipped to avoid loops and directory read errors.
- The database file and scan report are created with mode 0600 (owner read/write only).
- Database entries are normalized to absolute paths when loaded, so older databases with relative paths still work with directory-filtered modes such as `list-deleted` and `remove-deleted`.
- For `sync`, config directories are used as walk roots only. Which database entries get verified is still governed by the command line, so a config file can never silently narrow what gets checked.

### Examples

`checksumtool -mode sync ~/Pictures`

Verify everything in the database, discover new files under "Pictures", and report moves, additions, deletions and suspected corruption without changing anything.

`checksumtool -mode sync -manifest /var/tmp/checksum-observations.json ~/Pictures`

Write an opt-in machine-readable manifest alongside the ordinary scan report.
The manifest declares `schema_version: 1` independently of the checksum
database format. Each completed observation carries its path, category,
baseline when present, observed xxhash/size/mtime, source device/inode when
available, scan identity, and an observation identity. It is evidence only;
it does not authorize baseline changes or uploads/deletions. Interrupted scans
are marked partial and keep completed positive observations. Consumers may use
deletion observations only when that specific root's coverage is complete;
missing, unreadable and incomplete roots never establish absence.
Existing sync reports and default behavior are unchanged.

To accept one reviewed modified file, copy its `observation_id` from the
manifest and run:

```bash
checksumtool -mode apply-observations -manifest /var/tmp/checksum-observations.json -select OBSERVATION_ID -accept-suspect
```

The selected baseline entry is compared with the value recorded in that
observation. A concurrent unrelated database update does not invalidate it;
a changed file, a changed target entry, or an identity whose facts were edited
is refused. Unselected findings remain available in the manifest.
For a detected move, select the destination's `moved` observation; it checks
and updates the old and new baseline entries together. The matching
`move_source` observation cannot be applied on its own.

To reconsider a restored file without rescanning its directory:

```bash
checksumtool -mode recheck -db ~/.local/share/checksumtool/checksums.json -manifest /var/tmp/recheck.json ~/Pictures/restored.jpg
checksumtool -mode apply-observations -db ~/.local/share/checksumtool/checksums.json -manifest /var/tmp/recheck.json -select OBSERVATION_ID
```

Recheck manifests are deliberately marked partial and their root coverage is
`targeted`. They can provide positive evidence for the selected file, but never
prove that other paths are absent.

To verify a selected copied batch without changing the baseline:

```bash
checksumtool -mode verify-manifest -manifest /var/tmp/checksum-observations.json -select OBSERVATION_ID /mnt/staging
```

The alternate root preserves paths relative to the manifest's recorded source
root. Verify selected observations from multiple source roots in separate
invocations. Results check content digests and sizes; they do not claim that
the copy is a simultaneous snapshot of a changing source tree.

`checksumtool -mode apply`

Apply the findings of the last sync run to the database, without re-reading any files.

`checksumtool -mode sync -apply ~/Pictures`

Do both in a single run, suitable for cron.

`checksumtool -mode check ~/Documents ~/Pictures`

Compare checksums of files in the "Documents" and "Pictures" directories against the stored database.

`checksumtool -mode update -verbose ~/Projects`

Update the checksum database with files from the "Projects" directory and enable verbose output.

`checksumtool -mode list-missing ~/Music`

List files in the "Music" directory that are missing from the checksum database.

`checksumtool -mode add-missing -workers 8 ~/Videos`

Add checksums for missing files in the "Videos" directory to the database, using 8 worker goroutines.

`checksumtool -mode list-deleted`

List all files in the database that no longer exist on disk.

`checksumtool -mode remove-deleted ~/Pictures`

Remove deleted files under "Pictures" from the database.

## Database format

Each entry stores a checksum plus the size and modification time observed when that checksum was computed:

```json
{
  "checksums": {
    "/home/user/photos/a.jpg": {
      "checksum": 12297829382473034410,
      "size": 4823910,
      "mtime": 1723489200000000000
    }
  }
}
```

Size and timestamp may be absent, which means "unknown" and suppresses metadata-based inference for that entry. They are filled in by the first run that re-reads the file and confirms its checksum still matches.

Since v3.4 the database may also carry a top-level `"verified"` map of path to the Unix time its content was last read and matched. It drives rolling verification and is kept separate from the entries, so recording a read never makes an entry look changed. Older versions ignore it (and drop it when they rewrite the database, which only means those files count as never verified).

## Config File

checksumtool supports an optional TOML config file at `~/.config/checksumtool/config.toml` (override with `-config`). All fields are optional:

```toml
directories = [
    "/home/user/photos",
    "/home/user/documents",
]
workers = 8
verbose = true
strict_moves = false
verify_budget = "115G"   # used by sync -quick
```

**Precedence**: CLI flags always override config file values. If directories are passed as CLI arguments, config directories are ignored. If no CLI directories are provided, config directories are used only for `list-missing`, `add-missing` and `sync`; `check`, `update`, `list-deleted`, and `remove-deleted` scan the whole DB unless you explicitly pass directories. If the config file doesn't exist, defaults are used silently.

## Changes in v3.4

- **Quick sync and rolling verification**: `sync -quick` reads only new and changed files; `-verify-budget` adds a fixed amount of the least recently verified unchanged data per run. A daily `sync -quick -verify-budget <total/N> -apply` checks what changed every day and re-reads every file about every N days, without a long full pass. Verification times are recorded per path and survive interruption.
- **Selective application and path lists**: `-apply-categories` lets an unattended run record, say, additions and moves while leaving every content change and deletion for review. `-list-paths` writes per-category path lists for `rsync --files-from`, `tar -T` and similar tools.
- **`status` mode** and path-ordered reading (files are hashed in sorted path order, keeping each directory's reads together).

Example daily run, recording additions and moves and handing the day's new files to another tool:

```bash
checksumtool -mode sync -quick -verify-budget 100G -apply -apply-categories new,moved,metadata \
  -list-paths new=/tmp/new.txt,moved=/tmp/moved.txt -relative-to /home/user/photos /home/user/photos
rsync -a --files-from=/tmp/new.txt /home/user/photos/ /mnt/copy/
```

## Changes in v3.3

- **Stable reads and structured observations**: opt-in `sync -manifest` captures every completed observation, including clean files, source identity, root coverage and explicit errors. Unreadable or missing roots cannot create deletion decisions; completed positive evidence remains available from partial scans.
- **Selective apply and targeted recheck**: `apply-observations` applies only selected observation IDs with per-entry baseline comparisons, exact byte rechecks, idempotent results and atomic move updates. Suspect content needs explicit acceptance; deletion needs complete root coverage and a still-matching source root. `recheck` revisits selected files without a full tree scan.
- **Staging verification**: `verify-manifest` checks selected files in an alternate tree without changing the checksum database.

## Changes in v3.2

- **Silence when there is nothing to say**: a `sync` run that found nothing needing a decision writes no output at all when stdout is not a terminal. Previously it printed a summary and a reassuring verdict, which meant a monthly cron job mailed a report every month whether or not anything had happened.
- **`-verbose` requires a terminal**: informational chatter (`Loading checksum database...`, `Saving...`, `Comparing...`, `Finished operation...`) is suppressed off a terminal, including when `verbose = true` comes from the config file. Findings, warnings and errors are never suppressed. This lets one config serve both interactive and unattended use.

## Changes in v3.1

- **`sync` output**: only concerning findings are listed individually. Moves and additions still appear as counts in the summary and in full in the scan report; `-list-all` restores the per-file listing.
- **`sync` exit code**: a changed (`modified`) or `deleted` file now exits 1, alongside the corruption suspects and read errors that already did. Moves and additions never do. If you relied on `sync` exiting 0 through ordinary edits, pass over the summary counts instead of the exit status.

## Breaking Changes (v3)

- **Database format**: entries changed from a bare checksum number to an object carrying the checksum, size and modification time. Run `checksumtool -mode migrate` once to convert an existing database; every other mode refuses to load the old format and says so. Migration deliberately records no size or timestamp, because stat'ing during migration would pair a possibly-stale checksum with a fresh timestamp and fabricate the very claim the corruption check depends on. Until each file is next re-read and confirmed, its mismatches are reported as unverifiable rather than as corruption.
- **update**: no longer silently overwrites the stored checksum of a file whose mismatch looks like corruption. Pass `-with-corrupt` to accept those.

## Breaking Changes (v2)

- **Hash algorithm**: Switched from CRC32 to xxhash64 for better performance and collision resistance. Existing databases must be regenerated using `update` mode.
- **Default DB path**: Changed from `~/.local/lib/checksums.json` to `~/.local/share/checksumtool/checksums.json`.

## Attribution

checksumtool is developed by Toni Melisma and released in 2024. The code was almost entirely written by Claude based on my algorithm and instructions.

## Contributor Workflow

Repository changes are expected to complete the full Definition of Done automatically: implement the change, run `go test -v`, `go test -race -v`, and `go vet ./...`, update docs, commit, push, and create a patch release tag for user-facing bug fixes unless the handoff explicitly explains why no release was done.

The test suite is expected to stay hermetic: tests should use temp-only fixtures, isolate `HOME`/config/database paths when spawning the CLI, and never read from or write to a contributor's real checksumtool data.
