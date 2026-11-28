# 0002. Sync scans into a report; a separate apply writes the database

- Status: accepted
- Date: 2026-08-21

## Context and problem

A sync finds additions, deletions, moves and mismatches. Some of these need a human decision
(an unmounted volume makes everything under it look deleted; a mismatch may be rot), and moves
can be recognized only after the whole walk. Writing the database during the walk would either
apply decisions nobody made or force a second read of every file. Requirements: CT-030 to CT-036,
CT-060 to CT-062.

## Decision drivers

- No file should be read twice for one sync.
- Nothing that warrants a decision is applied by default.
- A move is the join of a vanished entry and a new file with the same checksum.

## Considered options

### Write the database while walking

- Good: one step.
- Bad: moves cannot be recognized mid-walk; deletions and suspect mismatches would be applied
  without review.
- Failure modes: an unmounted disk wipes its entries from the database.

### Two phases: walk into a scan report, then apply it (chosen)

- Good: `resolveMoves()` joins after the walk; `apply` re-stats each listed file and writes the
  recorded checksum only if size and modification time are unchanged, so nothing is re-read.
- Good: `apply` refuses a report whose database changed or whose scan was interrupted unless
  `-force`; deletions need `-with-deleted`; `sync -apply` does both in one process.
- Bad: a sidecar file and a second command.
- Failure modes: a stale report is refused rather than misapplied.

## Decision

Two phases, with the scan report (`<db>.scan.json`) as the hand-over and refusal rules in
`apply`. Moves become moves only when a checksum group has equal counts on both sides and sizes
agree; `-strict-moves` narrows this to one-to-one.

## Consequences

- Unattended runs can stay read-only and fail (exit 1) only on findings that need a human.
- The observation manifest workflow (`-manifest`, `apply-observations`) builds on the same split
  with per-item selection.

## Confirmation

`TestResolveMoves*`, `TestApplyRefusesStaleReport`, `TestVerifyScanReport`,
`TestApplyScanReportDeletionsNeedFlag`, `TestSyncDetectsAndAppliesMove` (CT-030 to CT-036).

## Retirement proof

A single-phase design that recognizes moves and defers every decision without a second read,
passing the same tests.

## Reversal plan

`sync -apply` already gives single-command use; removing the report would mean folding
`applyScanReport()` into `processResults()` and losing the refusal rules.
