# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Build & Test Commands

```bash
go build -o checksumtool          # Build binary
go test -v                        # Run all tests
go test -run TestFunctionName -v  # Run a single test
go test -race -v                  # Run tests with race detector
```

No Makefile, linter config, or CI pipeline exists — standard Go toolchain only.

Tests must remain hermetic. Use `t.TempDir()`/temp fixtures, isolate `HOME` and default config/data paths for subprocess CLI tests, and avoid touching a developer's real checksum database or config while running the suite.

## Architecture

Single-package Go CLI tool (`main.go`) that detects file corruption (bit rot) by computing xxhash64 checksums and storing them in a JSON database. External dependencies: `github.com/cespare/xxhash/v2`, `github.com/BurntSushi/toml`.

**Core data structures**:
- `FileEntry` — the database value: `{checksum, size, mtime}`. Size and mtime exist to tell corruption apart from an edit, and are never used to skip hashing (bit rot does not touch either). Zero means "unknown" and suppresses metadata-based inference for that entry.
- `ChecksumDB` — a map of absolute file paths to `FileEntry`, protected by a `sync.Mutex`. `loadChecksumDB()` normalizes any legacy relative keys to absolute paths on load, and refuses the pre-v3 bare-number format with a pointer to `-mode migrate`.
- `WorkerResult` — struct carrying file path, checksum, size, mtime, existence flag, and error from workers.
- `ScanReport` / `ScanFile` / `ScanMove` — what a `sync` run leaves behind so `apply` can write the database without re-reading files. Lists only changes, never the files that verified cleanly.

**Concurrency model**: Worker pool pattern using goroutines and channels. `worker()` goroutines read from a jobs channel, stat then hash each file, and send `WorkerResult`s to a results channel. `processResults()` consumes results and applies mode-specific logic. An `outputMu` mutex serializes stdout writes. Worker count is configurable (default: 4).

**Two-phase sync**: every mode except `sync` acts on each result as it arrives. `sync` cannot: a move is the join of a vanished database entry and a discovered file that share a checksum, and neither half means anything until the walk finishes. So `processResults` accumulates into `syncState`, and `resolveMoves()` performs the join afterwards. A checksum group becomes moves only when both sides have equal counts (any pairing then yields the same database) and recorded sizes agree; otherwise it falls back to deletions plus additions. `-strict-moves` narrows this to one-to-one.

**Mismatch classification**: `classifyMismatch()` returns corrupt (metadata unchanged), unverifiable (no recorded metadata) or modified (metadata changed). Corrupt and unverifiable are never written to the database without `-with-corrupt` — overwriting them destroys the only evidence of rot. This is why `update` no longer blindly overwrites changed files.

**Filesystem walking**: `list-missing`, `add-missing` and `sync` resolve directory arguments through symlinks before walking, skip symlinked directories found inside the walk to avoid loops and directory hashing errors, and still process symlinked files by hashing their targets.

**Operation modes** (`-mode` flag): `sync`, `apply`, `check`, `update`, `list-missing`, `add-missing`, `list-deleted`, `remove-deleted`, `migrate`. Directories are optional for DB-based modes (check, update, remove-deleted, list-deleted); required for filesystem-walking modes (list-missing, add-missing, sync). `apply` and `migrate` bypass the worker pipeline entirely.

**Scan/apply split**: `sync` writes a `ScanReport` sidecar (`<db>.scan.json`) and changes nothing; `apply` re-stats each listed file and writes the recorded checksum only if size and mtime are unchanged since the scan, so no file is read twice. The report records the database's size and mtime, and `apply` refuses a report whose database has changed or whose scan was interrupted unless `-force`. Deletions need `-with-deleted` because an unmounted volume makes every file under it look deleted. `sync -apply` runs both phases in one process and deletes the report afterwards.

**Directory semantics for sync**: config `directories` supply walk roots only. The database filter still comes from CLI arguments alone, so a config file can never silently narrow what gets verified.

**Config file**: Optional TOML config at `~/.config/checksumtool/config.toml` (override with `-config` flag). Supports `directories`, `workers`, `verbose`, and `strict_moves` fields. CLI flags always override config values; `flag.Visit()` detects explicitly-set flags. Config `directories` seed `list-missing`, `add-missing` and `sync` when no CLI directories are given, while DB-based modes only filter when directories are explicitly passed on the command line. Missing config file is silently ignored.

**Key flow**: `main()` → parse flags → `loadConfig()` → `loadChecksumDB()` → `getFilesToProcess()` (or `getSyncFiles()`) → spawn workers → `processResults()` → for sync, `buildScanReport()` → `saveScanReport()` and optionally `applyScanReport()` → `saveChecksumDB()`. Context-based interrupt handling via `context.WithCancel` stops job feeding on SIGINT/SIGTERM and saves the DB for mutating modes. An interrupted sync marks its report `partial`, which `apply` refuses without `-force` — a partial scan sees files as deleted that it never reached.

**Default DB path**: `~/.local/share/checksumtool/checksums.json` (created with mode 0600). Scan reports default to `<db-dir>/<db-basename>.scan.json`, also 0600. Both are written through `writeFileAtomic()` (temp file + rename).

**Exit codes**: 0 on success, 1 on checksum mismatches in check mode, suspected corruption in sync mode, or on errors. Moves, additions and deletions are not errors.

## Definition of Done

Every feature or change is complete when all of the following are satisfied:

1. **Code**: Implementation in `main.go` (or new files if needed)
2. **Tests**: Unit and integration tests pass — `go test -v`, `go test -race -v`, `go vet ./...`
3. **Docs**: Update `README.md` (flags, usage examples, feature list) and `CLAUDE.md` (architecture notes)
4. **Commit**: Create a git commit with a descriptive message
5. **Push**: Push to the remote repository
6. **Tag & Release**: User-facing changes should normally be released. Create and push a version tag for:
   - Bug fixes and behavior corrections: patch release (for example `v1.1.1`)
   - Backward-compatible features or meaningful UX improvements: minor release (for example `v1.2.0`)
   - Breaking changes: major release
   If a change is intentionally not being released yet, say that explicitly in the handoff instead of silently skipping the release step.

## Execution Expectations

- Do not stop at implementation. Unless the user explicitly says otherwise, continue automatically through tests, docs, commit, push, and release work required by the Definition of Done.
- Do not ask for permission for ordinary repository tasks that are already part of the Definition of Done. Execute them.
- For user-facing bug fixes and behavior corrections, create and push the patch release tag automatically. Do not leave a fix unreleased just because the user did not separately ask for a release.
- Before handing off, perform a full Definition of Done audit in order. If any item is incomplete, stop, complete it, and then restart the audit from item 1. Repeat until every Definition of Done item is satisfied.
- End every completed task with a compact Definition of Done report covering:
  - code changes made
  - verification commands and results
  - docs updated
  - commit SHA
  - push status
  - release tag status, or an explicit reason no release was done
