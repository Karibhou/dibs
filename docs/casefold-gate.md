# GitHub identifier comparison audit

GitHub owner, repository and login identifiers are case-insensitive. Preserve
stored/display spelling; use `strings.EqualFold` for identity comparisons or
`strings.ToLower` for ephemeral set/map keys on both insertion and lookup.
Registry lookups already resolve to the registered spelling.

`TestRepoIDCasefoldGate` scans non-test Go source under `pkg/` and `cmd/` in the
existing test suite. It reports raw identifier equality/inequality, switch tags,
and map-key-shaped index expressions. Empty-string checks are safe. Reviewed
exact sites have a `file:line` allowlist with individual reasons; line drift
requires re-review, not a broad exemption. This is syntactic, not type/data-flow
analysis: aliases, wrappers, already-normalised locals and canonical registry
values still need human review.

Allowlist entries are pinned to `file:line`, so an edit above a reviewed site
moves it off its entry. The gate then **fails in every mode** with the stale
entries listed: re-pin each one to the current line (re-reading the reason
against the new code) or delete it. Without that check the site resurfaces as
an unreviewed hit that an advisory `go test` run without `-v` never prints.

The gate is **advisory by default**. See its diagnostics with:

```sh
go test -count=1 -v ./pkg/store -run '^TestRepoIDCasefoldGate$'
```

Opt in locally to failing on unreviewed hits:

```sh
DIBS_CASEFOLD_GATE=strict go test -count=1 -v ./pkg/store -run '^TestRepoIDCasefoldGate$'
```

## CI observation and ratchet

CI runs the audit again with `-count=1 -v` directly after the test suite and
copies its full output to both the CI log and job summary. This is a step in
the existing job. A passing non-verbose `go test` discards `t.Logf` output, so
runs since the original 2026-10-23 rollout supplied no usable observations.
The observation clock restarts after #508 merged on 2026-10-28, once this
visible CI step lands; do not count the earlier period as zero hits.

The recorded baseline is **0 unreviewed sites**, measured at `main@1938d81`
(#508), and stored as `casefoldBaseline` in the gate test. New unreviewed sites
must be reviewed or fixed, rather than accepted by raising the baseline.

Per the decision on #509, CI ran with `DIBS_CASEFOLD_GATE=ratchet-warning` from
2026-10-28 (first visible audit after #508) through 2026-11-11. Dibs publishes
a commit-sha image after each green `main` CI run rather than using tagged
releases (see [the release runbook](../runbooks/release-rollback.md#how-releases-reach-the-cluster)).
After main run [43046057372](https://github.com/hivecommons/dibs/actions/runs/43046057372)
(commit 6e7101a9c700a29793091bd280c6d0760ebfa518, 2026-11-11T15:12:40Z) confirmed
0 unreviewed sites following #648's normalisation of 8 casefold-mismatched sites,
enforcement was enabled: CI now runs `DIBS_CASEFOLD_GATE=ratchet`.

That mode fails only when the unreviewed count exceeds the recorded baseline (equal or
lower passes).
It does not change the local advisory default. `strict` continues to fail on
any unreviewed site, independently of the baseline.

Only an unset/empty value, `ratchet-warning`, `ratchet`, and `strict` are valid
for `DIBS_CASEFOLD_GATE`. Unknown values fail the test with a configuration
error, even when there are no unreviewed sites.
