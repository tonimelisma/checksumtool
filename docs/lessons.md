# Lessons

Rules learned the hard way that the code alone doesn't explain, and problems still open. Entries
are deleted once resolved or captured in a test, an ADR or CLAUDE.md.

## Rules learned

- Cron mails whatever a job writes, so a report that always arrives stops being read. `sync`
  off a terminal says nothing when nothing needs a decision, and config `verbose = true` cannot
  bring the chatter back (CT-061).
- An unmounted volume makes every file under it look deleted. Deletions are never applied
  without `-with-deleted`, and a manifest root without complete coverage reports
  `unconfirmed_absence` (CT-035, CT-051).
- A config file must never silently narrow what gets verified: config `directories` seed walk
  roots only, never the database filter (CT-037).
- Databases written by early versions hold relative paths; normalizing them on load is what
  keeps `list-deleted` scoped to a directory correct (CT-002).
- Every file is read once per scan, so the page cache gains nothing from keeping it; dropping
  pages after hashing keeps other programs' working sets (CT-013).

## Open problems

- Signal handling (CT-038), read order (CT-012), page-cache advice (CT-013) and hashing a
  symlinked file's target (CT-014) have no test.
