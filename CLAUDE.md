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

## Architecture

Single-package Go CLI tool (`main.go`) that detects file corruption (bit rot) by computing xxhash64 checksums and storing them in a JSON database. External dependencies: `github.com/cespare/xxhash/v2`, `github.com/BurntSushi/toml`.

**Core data structures**:
- `ChecksumDB` — a map of absolute file paths to uint64 checksums, protected by a `sync.Mutex`.
- `WorkerResult` — struct carrying file path, checksum, existence flag, and error from workers.

**Concurrency model**: Worker pool pattern using goroutines and channels. `worker()` goroutines read from a jobs channel, compute checksums, and send `WorkerResult`s to a results channel. `processResults()` consumes results and applies mode-specific logic. An `outputMu` mutex serializes stdout writes. Worker count is configurable (default: 4).

**Operation modes** (`-mode` flag): `check`, `update`, `list-missing`, `add-missing`, `list-deleted`, `remove-deleted`. Directories are optional for DB-based modes (check, update, remove-deleted, list-deleted); required for filesystem-walking modes (list-missing, add-missing).

**Config file**: Optional TOML config at `~/.config/checksumtool/config.toml` (override with `-config` flag). Supports `directories`, `workers`, and `verbose` fields. CLI flags always override config values; `flag.Visit()` detects explicitly-set flags. Missing config file is silently ignored.

**Key flow**: `main()` → parse flags → `loadConfig()` → `loadChecksumDB()` → `getFilesToProcess()` → spawn workers → `processResults()` → `saveChecksumDB()`. Context-based interrupt handling via `context.WithCancel` stops job feeding on SIGINT/SIGTERM and saves the DB for mutating modes.

**Default DB path**: `~/.local/share/checksumtool/checksums.json` (created with mode 0600).

**Exit codes**: 0 on success, 1 on checksum mismatches in check mode or on errors.

## Definition of Done

Every feature or change is complete when all of the following are satisfied:

1. **Code**: Implementation in `main.go` (or new files if needed)
2. **Tests**: Unit and integration tests pass — `go test -v`, `go test -race -v`, `go vet ./...`
3. **Docs**: Update `README.md` (flags, usage examples, feature list) and `CLAUDE.md` (architecture notes)
4. **Commit**: Create a git commit with a descriptive message
5. **Push**: Push to the remote repository
6. **Tag & Release**: If the change warrants a version bump, create a git tag (e.g. `v1.1.0`) and push it
