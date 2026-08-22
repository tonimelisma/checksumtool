# checksumtool

checksumtool is a command-line utility for calculating and comparing file checksums. It allows you to efficiently manage checksums for a large number of files and detect any changes or mismatches.

My personal use case is to detect bit rot in any pictures. Throughout the years old photos get corrupted. This utility detects corrupted photos, allowing me to restore them from backups.

## Features

- Calculate checksums for files in one or more directories
- Compare checksums against a stored database to detect changes or mismatches
- Tell suspected corruption apart from ordinary edits, using the recorded size and timestamp
- Detect moved files and relocate their database entry instead of reporting a deletion plus an addition
- Scan once, review, then apply without re-reading a single file
- Update the checksum database with new or changed files
- List files that are missing from the checksum database
- Add checksums for missing files to the database
- List and remove deleted files from the database
- Optional TOML config file for default directories, workers, and verbose settings
- Progress tracking and estimation of remaining time
- Interrupt handling to save work done so far
- Non-zero exit code on checksum mismatches in check and sync modes

## Usage
checksumtool [flags] [directories...]

`checksumtool -help` prints the full mode reference, including how each kind of mismatch is classified.

### Flags
- `-db string`: Checksum database file location (default `$HOME/.local/share/checksumtool/checksums.json`)
- `-config string`: Config file location (default `$HOME/.config/checksumtool/config.toml`)
- `-verbose`: Enable verbose output
- `-mode string`: Operation mode: sync, apply, check, update, list-missing, add-missing, remove-deleted, list-deleted, migrate
- `-workers int`: Number of worker goroutines (default 4)
- `-scan-report string`: Scan report location (default: alongside the database, as `<db>.scan.json`)
- `-apply`: With `sync`, write the scan results to the database in the same run
- `-with-deleted`: When applying, also remove entries whose file is gone
- `-with-corrupt`: When updating or applying, also accept content changes that could not be told apart from corruption
- `-strict-moves`: Only treat a checksum group as a move when exactly one file vanished and one appeared
- `-force`: With `apply`, use the scan report even if it is partial or the database changed since the scan

### Operation Modes
- `sync`: One pass over everything. Verifies database entries, walks the directories for files not yet recorded, and classifies each file as verified, modified, corrupt, moved, new or deleted. Reports only and writes a scan report; add `-apply` to write the results in the same run. Requires at least one directory (from arguments or the config file). Exits with code 1 if corruption is suspected or a file could not be read.
- `apply`: Apply a scan report written by a previous `sync` run, without re-reading any file contents. Applies moved, new, modified and metadata-only changes; deletions and corruption suspects need `-with-deleted` / `-with-corrupt`.
- `check`: Compare checksums of files against the stored database and report any mismatches. Exits with code 1 if any mismatches are found. Directories are optional; if omitted, all DB entries are checked.
- `update`: Update the checksum database with new or changed files. Refuses to overwrite an entry whose mismatch looks like corruption unless `-with-corrupt` is passed. Directories are optional.
- `list-missing`: List files that are missing from the checksum database. Requires at least one directory.
- `add-missing`: Add checksums for missing files to the database. Requires at least one directory.
- `remove-deleted`: Remove files from the database that no longer exist on disk. Directories are optional.
- `list-deleted`: List files in the database that no longer exist on disk. Directories are optional.
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
```

**Precedence**: CLI flags always override config file values. If directories are passed as CLI arguments, config directories are ignored. If no CLI directories are provided, config directories are used only for `list-missing`, `add-missing` and `sync`; `check`, `update`, `list-deleted`, and `remove-deleted` scan the whole DB unless you explicitly pass directories. If the config file doesn't exist, defaults are used silently.

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
