# Requirements

What checksumtool does today. Each requirement is one EARS sentence with a stable `CT-NNN` ID,
marked `[verified]` when tests tagged `Validates: CT-NNN` prove it, or `unverified:` with the
reason. Future work lives in GitHub issues. `script/process-check` enforces the format.

## Database

- **CT-001** The tool shall record each file's xxhash64 checksum, size and modification time in a
  JSON database keyed by absolute path, written atomically (temporary file and rename) with mode
  0600. [verified]
- **CT-002** When the database holds relative paths, the tool shall normalize them to absolute
  paths on load. [verified]
- **CT-003** If the database is in the pre-v3 bare-number format, then the tool shall refuse it
  and name `-mode migrate`, which converts it in place. [verified]
- **CT-004** While a mode that writes the database runs, the tool shall hold an exclusive lock on
  it, so a second writer fails instead of interleaving. [verified]
- **CT-005** The tool shall keep verification times outside the database entries, so a read time
  never makes an entry compare as changed and older readers ignore them. [verified]
- **CT-006** Where no `-db` is given, the tool shall use
  `~/.local/share/checksumtool/checksums.json`, and `<db-dir>/<db-basename>.scan.json` for the
  scan report. [verified]

## Reading files

- **CT-010** If a file's identity, size, modification time, mode or change time differs before
  and after hashing, or its path no longer resolves to the opened file, then the tool shall report
  an error instead of a checksum. [verified]
- **CT-011** When walking directories, the tool shall resolve root arguments through symlinks
  and skip symlinked directories inside the walk. [verified]
- **CT-012** The tool shall hash the job list in path order, so a directory's files are read
  together. unverified: no test observes the order in which workers read files; the sort happens
  in `main()` before the workers start.
- **CT-013** While running on Linux, the tool shall advise `POSIX_FADV_SEQUENTIAL` before reading
  a file and `POSIX_FADV_DONTNEED` after hashing it, best effort and without changing results.
  unverified: the advice has no observable effect a hermetic test can assert; CI runs the code
  path on Linux with every read.
- **CT-014** When a walk finds a symlink to a file, the tool shall hash the link's target.
  unverified: no test walks a symlink to a regular file; only a dangling link (a read error) is
  tested.

## Mismatches

- **CT-020** When a checksum does not match, the tool shall classify the file as corrupt (size
  and modification time unchanged), unverifiable (no recorded metadata) or modified (metadata
  changed). [verified]
- **CT-021** The tool shall not write a corrupt or unverifiable checksum to the database unless
  `-with-corrupt` is given, in `update` and in `apply`. [verified]

## Sync

- **CT-030** When a vanished database entry and a newly found file share a checksum, the tool
  shall report a move only if both sides of that checksum group have equal counts and recorded
  sizes agree, and report deletions plus additions otherwise. [verified]
- **CT-031** Where `-strict-moves` is set, the tool shall report moves only for one-to-one
  checksum groups. [verified]
- **CT-032** When `sync` runs without `-apply`, the tool shall write a scan report listing only
  changes and leave the database unchanged. [verified]
- **CT-033** When `apply` runs, the tool shall write a reported checksum only if the file's size
  and modification time are unchanged since the scan, and skip a move whose source has returned.
  [verified]
- **CT-034** If the database changed since the scan or the scan was interrupted, then `apply`
  shall refuse the report unless `-force` is given. [verified]
- **CT-035** The tool shall apply deletions only under `-with-deleted`, and keep a deleted entry
  whose file is back. [verified]
- **CT-036** When `sync -apply` runs, the tool shall scan and apply in one process. [verified]
- **CT-037** The tool shall take sync walk roots from config `directories` when none are given on
  the command line, but filter database-based modes only by command-line directories. [verified]
- **CT-038** If the process receives SIGINT or SIGTERM, then the tool shall stop feeding jobs,
  save the database for writing modes, and mark a sync's report partial.
  unverified: no test delivers a signal to a running scan; `verifyScanReport` refusing a partial
  report is covered by CT-034.

## Quick sync and rolling verification

- **CT-040** When `sync -quick` runs, the tool shall read only files that are gone, changed size
  or modification time, or have unknown metadata, plus the unchanged files verified longest ago
  within the verify budget, and report the rest as unread. [verified]
- **CT-041** The tool shall accept `-verify-budget` as bytes (`2G`, `1.5G`, `1TiB`) or as a
  percentage of the in-scope recorded bytes resolved on each run, and refuse anything else.
  [verified]
- **CT-042** When a run writes the database, the tool shall record the verification time of each
  file it read and found matching. [verified]
- **CT-043** The tool shall refuse unknown `-apply-categories` and write per-category path lists,
  leaving out (and reporting) any path containing a line break. [verified]

## Observation manifests

- **CT-050** When `sync -manifest PATH` runs, the tool shall write a schema-v1 manifest recording
  every completed observation, verified files included, each with an identity bound to its
  classification and facts. [verified]
- **CT-051** If a root's coverage is not complete, then the tool shall report absence under it as
  `unconfirmed_absence`, never as a deletion, and keep the positive observations it made.
  [verified]
- **CT-052** When `apply-observations` runs, the tool shall re-read each selected file and apply
  it only if it still matches its expected baseline; suspect bytes need `-accept-suspect`,
  deletions need `-with-deleted` and complete root coverage, and a move applies both sides
  together or not at all. [verified]
- **CT-053** The tool shall never let `recheck` evidence authorize a deletion. [verified]
- **CT-054** When `verify-manifest` runs, the tool shall compare selected observations against an
  alternate root and never write the database. [verified]

## Reporting and exit codes

- **CT-060** When `sync` reports, the tool shall list concerning findings individually and reduce
  moves and additions to counts, unless `-list-all` is given. [verified]
- **CT-061** While stdout is not a terminal, if a complete sync found nothing concerning, then the
  tool shall print nothing at all, and progress output shall stay off even when `-verbose` or
  config `verbose` asks for it. [verified]
- **CT-062** The tool shall exit 1 on check-mode mismatches, on errors, and on any concerning
  sync finding (modified, corrupt, unverifiable, deleted), and exit 0 when a sync found only
  moves and additions. [verified]

## Command line and config

- **CT-070** If `-mode` is missing, `-workers` is not positive, or a directory-walking mode
  gets no directories, then the tool shall exit non-zero with a message; `-help` shall describe
  the modes, the safety flags and the exit codes. [verified]
- **CT-071** The tool shall read optional TOML config from `~/.config/checksumtool/config.toml`
  (or `-config`), ignore a missing file, refuse an invalid one, and let explicit flags override
  it. [verified]
- **CT-072** When `-version` or `--version` is given, the tool shall print
  `checksumtool vX.Y.Z` (the stamped version, else the module version from Go's build info) and
  exit 0. [verified]

## Releases

- **CT-080** When CI succeeds for a push to main, the Release workflow shall publish the next
  patch version with static linux-amd64 and linux-arm64 binaries, `SHA256SUMS` and a build
  provenance attestation, then download the release and verify its checksums and attestation
  against this repository's release workflow on `refs/heads/main`. [verified]
- **CT-081** If a release would run from a ref other than main, for a version not higher than the
  highest tag, or for a commit that does not contain the highest tag, then the Release workflow
  shall refuse it. unverified: these guards in `.github/workflows/release.yml` only run when the
  condition occurs; no test exercises them.
