# 0003. Releases are published only by a workflow, from CI-green main, with provenance attestation

- Status: accepted
- Date: 2026-11-24

## Context and problem

Binaries are installed on machines that should run only code that passed CI on main. A release
made by hand, or from a tag push, carries no proof of where it was built. Cloud sessions also
cannot push tags or create releases. Requirements: CT-080, CT-081.

## Decision drivers

- Every installed binary must be traceable to a reviewed, tested commit on main.
- No manual step between merge and release.
- Consumers need a check they can run themselves.

## Considered options

### Tag-triggered release workflow

- Good: common practice.
- Bad: anyone who can push a tag can release any commit, tested or not.
- Failure modes: an untested or out-of-order commit becomes the latest release.

### Release on `workflow_run` of CI for pushes to main, attested (chosen)

- Good: the commit CI tested is the one built; the workflow picks the next patch version, tags
  it with `GITHUB_TOKEN`, builds static linux-amd64/arm64 binaries, writes `SHA256SUMS`, attests
  them, publishes, then verifies the published release with `gh attestation verify`.
- Good: refuses refs other than main, versions not higher than the highest tag, and commits that
  don't contain it; a `release` concurrency group serializes runs; a rerun finishes a release.
- Bad: a minor or major version needs a `workflow_dispatch` with the version.
- Failure modes: a failed release leaves a draft that the next run finishes.

## Decision

`.github/workflows/release.yml` is the only workflow that tags, releases or attests, triggered by
CI succeeding for a push to main (or a dispatch on main), and nothing triggers on tags.

## Consequences

- Merging to main is releasing; an unready change must not be merged.
- Consumers verify with `gh attestation verify --signer-workflow
  <repo>/.github/workflows/release.yml --source-ref refs/heads/main`.

## Confirmation

The workflow's own "Verify the published release" step on every release (CT-080).

## Retirement proof

A different release mechanism that keeps the same attestation subject and verification, shown
with a published release that passes `gh attestation verify`.

## Reversal plan

Re-enable manual releases by dispatch only; attestations stay tied to the workflow file.
