# Publication checkout verification

These checks ran on the prepared publication source, distinct from historical
candidate build receipts. Go 1.27.0 Windows, one idle CPU, cached modules only.
Go broker/adapter/healthcheck regressions, vet/module verification, five builds
including Linux cross-build, Python packing/compiled CLI contract and Node
migration/operator/release/dashboard contracts were checked. The inherited
Windows symlink/POSIX fixtures remain environment-blocked (54 migration and13
operator tests passed, four fixtures blocked); operator private-backup
qualification is unpassed on this host.
blocked/skipped checks are not passes. No new model calls,
services, heldout reads, dependency downloads or deployment occurred.

The JSON contains commands, return codes, log hashes, durations, build hashes
and explicit qualification limits. Test logs/binaries/caches remain local,
outside source. Source implementations remain byte-identical to the pinned
original; portable path/documentation/research adaptations are listed in root
provenance and omissions. VCS build metadata was disabled for this pre-commit
verification. The later publication commit is verified separately by Git tree.
