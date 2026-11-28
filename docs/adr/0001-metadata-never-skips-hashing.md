# 0001. Size and modification time classify mismatches; they never skip hashing

- Status: accepted
- Date: 2026-08-21

## Context and problem

checksumtool exists to find bit rot: content that changed while the filesystem's metadata did
not. Recording size and modification time is cheap and tells an edit apart from corruption, but
the obvious optimization (skip files whose metadata is unchanged) would blind the tool to exactly
the failure it exists for. Requirements: CT-020, CT-021, CT-040, CT-041.

## Decision drivers

- Bit rot leaves size and modification time untouched, so only reading the bytes can see it.
- The only evidence of rot is the old checksum; overwriting it destroys the evidence.
- Large archives make a full read of every file on every run slow.

## Considered options

### Skip unchanged files (rsync-style quick check)

- Good: fast; reads only what changed.
- Bad: never detects corruption, the tool's purpose.
- Failure modes: silent rot that is discovered only when the backup also holds the rotted copy.

### Always hash everything; metadata only classifies (chosen), with an opt-in rolling budget

- Good: every run of plain `sync` sees all rot; a mismatch with unchanged metadata is reported as
  likely corruption and is never written over without `-with-corrupt`.
- Good: `sync -quick` with `-verify-budget` (bytes or a percentage of in-scope bytes) reads the
  changed files plus the longest-unverified rest, so a fixed cycle still reads everything.
- Bad: a full sync costs a full read.
- Failure modes: with `-quick`, rot is found within one budget cycle rather than on the next run.

## Decision

Always hash; size and modification time classify a mismatch as corrupt, unverifiable or modified
and never skip a read. `-quick` is the one exception, opt-in, and the verify budget restores full
coverage over a cycle whose length stays fixed as data grows.

## Consequences

- `update` and `apply` refuse to overwrite corrupt or unverifiable entries unless told to.
- Verification times live outside the entries (CT-005) so rotation never changes comparisons.

## Confirmation

`TestClassifyMismatch`, `TestProcessResultsUpdateRefusesSuspectedCorruption`,
`TestApplyScanReportCorruptNeedsFlag`, `TestQuickSelectRotatesByVerificationAge` and
`TestPercentVerifyBudget` (CT-020, CT-021, CT-040, CT-041).

## Retirement proof

A filesystem with end-to-end content checksums (ZFS, Btrfs scrub) covering every archive the
tool watches, shown to report the same corruption the tool's tests inject.

## Reversal plan

Not reversible without giving up corruption detection; the budget can be raised to 100% to make
`-quick` read everything.
