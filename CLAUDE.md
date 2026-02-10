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

Single-package Go CLI tool (`main.go`) that detects file corruption (bit rot) by computing xxhash64 checksums and storing them in a JSON database. One external dependency: `github.com/cespare/xxhash/v2`.

**Core data structures**:
- `ChecksumDB` — a map of absolute file paths to uint64 checksums, protected by a `sync.Mutex`.
- `WorkerResult` — struct carrying file path, checksum, existence flag, and error from workers.

**Concurrency model**: Worker pool pattern using goroutines and channels. `worker()` goroutines read from a jobs channel, compute checksums, and send `WorkerResult`s to a results channel. `processResults()` consumes results and applies mode-specific logic. An `outputMu` mutex serializes stdout writes. Worker count is configurable (default: 4).

**Operation modes** (`-mode` flag): `check`, `update`, `list-missing`, `add-missing`, `list-deleted`, `remove-deleted`. Directories are optional for DB-based modes (check, update, remove-deleted, list-deleted); required for filesystem-walking modes (list-missing, add-missing).

**Key flow**: `main()` → parse flags → `loadChecksumDB()` → `getFilesToProcess()` → spawn workers → `processResults()` → `saveChecksumDB()`. Context-based interrupt handling via `context.WithCancel` stops job feeding on SIGINT/SIGTERM and saves the DB for mutating modes.

**Default DB path**: `~/.local/share/checksumtool/checksums.json` (created with mode 0600).

**Exit codes**: 0 on success, 1 on checksum mismatches in check mode or on errors.
