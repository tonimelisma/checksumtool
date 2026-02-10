# checksumtool

checksumtool is a command-line utility for calculating and comparing file checksums. It allows you to efficiently manage checksums for a large number of files and detect any changes or mismatches.

My personal use case is to detect bit rot in any pictures. Throughout the years old photos get corrupted. This utility detects corrupted photos, allowing me to restore them from backups.

## Features

- Calculate checksums for files in one or more directories
- Compare checksums against a stored database to detect changes or mismatches
- Update the checksum database with new or changed files
- List files that are missing from the checksum database
- Add checksums for missing files to the database
- List and remove deleted files from the database
- Optional TOML config file for default directories, workers, and verbose settings
- Progress tracking and estimation of remaining time
- Interrupt handling to save work done so far
- Non-zero exit code on checksum mismatches in check mode

## Usage
checksumtool [flags] [directories...]

### Flags
- `-db string`: Checksum database file location (default `$HOME/.local/share/checksumtool/checksums.json`)
- `-config string`: Config file location (default `$HOME/.config/checksumtool/config.toml`)
- `-verbose`: Enable verbose output
- `-mode string`: Operation mode: check, update, list-missing, add-missing, remove-deleted, list-deleted
- `-workers int`: Number of worker goroutines (default 4)

### Operation Modes
- `check`: Compare checksums of files against the stored database and report any mismatches. Exits with code 1 if any mismatches are found. Directories are optional; if omitted, all DB entries are checked.
- `update`: Update the checksum database with new or changed files. Directories are optional; if omitted, all DB entries are re-checked.
- `list-missing`: List files that are missing from the checksum database. Requires at least one directory.
- `add-missing`: Add checksums for missing files to the database. Requires at least one directory.
- `remove-deleted`: Remove files from the database that no longer exist on disk. Directories are optional; if omitted, all DB entries are checked.
- `list-deleted`: List files in the database that no longer exist on disk. Directories are optional; if omitted, all DB entries are checked.

### Notes

- Symlinks are followed by default (Go's `filepath.Walk` behavior). Symlink loops may cause issues.
- The database file is created with mode 0600 (owner read/write only).

### Examples

`checksumtool -mode check ~/Documents ~/Pictures`

Compare checksums of files in the "Documents" and "Pictures" directories against the stored database.

`checksumtool -mode check`

Check all files in the database regardless of directory.

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

## Config File

checksumtool supports an optional TOML config file at `~/.config/checksumtool/config.toml` (override with `-config`). All fields are optional:

```toml
directories = [
    "/home/user/photos",
    "/home/user/documents",
]
workers = 8
verbose = true
```

**Precedence**: CLI flags always override config file values. If directories are passed as CLI arguments, config directories are ignored. If the config file doesn't exist, defaults are used silently.

## Breaking Changes (v2)

- **Hash algorithm**: Switched from CRC32 to xxhash64 for better performance and collision resistance. Existing databases must be regenerated using `update` mode.
- **Default DB path**: Changed from `~/.local/lib/checksums.json` to `~/.local/share/checksumtool/checksums.json`.

## Attribution

checksumtool is developed by Toni Melisma and released in 2024. The code was almost entirely written by Claude based on my algorithm and instructions.
