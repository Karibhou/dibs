# Operator handoff: production update #558

**Prepared, not deployed.** Only an authorized OKE operator may execute this
rollout. Keep [#558](https://github.com/hivecommons/dibs/issues/558) open until
the operator records the results below. The owner-approved weekly reviewed
cadence and out-of-cycle important fixes are tracked separately in
[#567](https://github.com/hivecommons/dibs/issues/567); neither tracker
permits deploying `latest` or unattended cluster writes.

## Selected release and evidence

Selected green `main` at preparation time:
`d411cb967f802c0515327fe302de3c1de5fe8409` (not the older sha proposed in the
original issue). Successful runs for that exact sha:

- [CI](https://github.com/hivecommons/dibs/actions/runs/41492320287)
- [Go Security](https://github.com/hivecommons/dibs/actions/runs/41492320327)
- [OpenSSF Scorecard](https://github.com/hivecommons/dibs/actions/runs/41492321809)
- [Docker publication](https://github.com/hivecommons/dibs/actions/runs/41492468755)

GHCR's full-sha tag resolved to the following OCI index. Its response digest
was independently checked against SHA-256 of the raw manifest body:

```text
ghcr.io/hivecommons/dibs@sha256:30db26fa8f5c7f870e61892d81f6abe4b3b2ecdb2a455a51f3fe0eedc8995f68
linux/amd64: sha256:30e6c8dc738ddfe2a4750f30de0725bbe399aca9e7bced77db13e1510ddfae16
linux/arm64: sha256:9443308521284c0444cbab8c22a9b2ba834775c03009d37be782c56a86059ec0
```

Both platform manifests and their config blobs were fetched and their hashes
checked. This establishes registry integrity and platform availability, not
runtime acceptance or an image signature verification. The Deployment pins
the multi-platform index, not a moving tag. Recheck the linked runs and image
availability before execution; selecting a different release requires a new
review of the digest and compatibility delta.

Read-only public checks during preparation returned HTTP 200 at both
`https://dibs.hivecommons.dev/healthz` and `/readyz`, with version
`f55e314c8662cbeb1056e01a76602c5e46f2df86`. This is evidence of the stale
release, not the cluster's actual image digest or a successful update.

## Actual persistence/rollback review

Reviewed the source delta from that public version to the selected sha,
including `pkg/store`, `pkg/notify`, `pkg/registry`, `pkg/history`, `pkg/news`,
`pkg/catalog`, and the shared `pkg/fsutil` writer, against
[release-rollback §2](../runbooks/release-rollback.md#2-check-for-data-changes-first).
This is not a claim that all persistence behavior is unchanged:

- Ideas remain `ideas/<id>.json` with `ideas/index.json`; the index entry
  layout is unchanged. `Idea` adds optional `cncfCatalogHash` (#521). Missing
  hashes invalidate derived suggestions; old readers use `json.Unmarshal`
  and tolerate unknown fields. Old writers can drop the hash, causing
  recomputation after re-upgrade, not an idea schema migration.
- #562 rewrites the index at startup to prune entries whose idea files are
  missing, as well as adopting orphans. #564 skips removed siblings in the
  duplicate-issue scan. **Inspect unexplained missing files before starting
  the new image**: this repair is not undone by an image rollback.
- Idea deletion now restores the previous file/index entry on index-write
  failure. Unicode length validation and case-insensitive offer lookup
  change behavior, not the persisted layout.
- Notifications and repo profiles retain their JSON arrays and paths;
  persistence now uses the shared atomic JSON writer. History/news retain
  their layouts and restore in-memory state after failed writes. News may
  regenerate fallback cards; catalog refresh carries enrichment forward
  and rejects an empty replacement. These are derived-data changes, not
  evidence that old code reproduces the new matching results.

No incompatible required-field or path migration was found. Image-only
rollback is expected to remain readable, but is **not byte-for-byte data
rollback**: the new process can rewrite indexes and caches, and old writers
can remove optional fields. Back up the entire data volume, not just ideas.
If the operator finds a different running version, extra writers, or an
unexplained inconsistent store, stop and re-review before proceeding.

## Operator gates (before any image change)

1. Verify the intended cluster/context and namespace. Capture the current
   Deployment YAML, rollout history, actual container name/image, pod
   `imageID`s, replica count and public health/readiness output in the
   restricted operational record. Do not post secrets or user data to GitHub.
   For example, these are read-only:

   ```sh
   kubectl config current-context
   kubectl -n dibs get deploy/dibs -o yaml
   kubectl -n dibs rollout history deploy/dibs
   kubectl -n dibs get pods -l app=dibs -o wide
   kubectl -n dibs get pods -l app=dibs -o jsonpath='{range .items[*]}{.metadata.name}{" "}{.status.containerStatuses[*].imageID}{"\n"}{end}'
   ```

2. Resolve and retain the **actual previous immutable image**, including
   architecture. Ensure it remains pullable and record a tested rollback
   command using that digest. A health sha alone is not the previous image
   digest. Do not assume this handoff knows the cluster's image or revision.
3. Follow [data recovery](../runbooks/data-recovery.md) and obtain a verified,
   access-controlled backup outside the cluster, including all `/data`
   files, permissions and ownership (65532). Validate archive contents and
   recovery procedure. The production image is distroless: **do not assume
   `kubectl exec ... tar` works**. Use the operator's approved storage
   snapshot/backup tooling or an approved volume-mounted backup pod.
   Atomic writes only ensure per-file consistency; use a quiesced writer or
   the storage team's application-consistent snapshot procedure for a
   coherent cross-file backup. Coordinate any maintenance window explicitly.
   Do not delete/recreate the PVC or restore over a live writer.
4. Confirm the backup and store inspection are satisfactory, normal replica
   count is one, and no other rollout is active. Existing RollingUpdate can
   briefly overlap old/new processes on RWX storage; it is not a cross-process
   locking mechanism. Avoid user mutations during overlap, or use an
   operator-approved quiesced maintenance procedure. Do not introduce extra
   steady-state replicas for this single-writer JSON store.

## Execute and verify (operator only)

After all gates pass, use the exact reviewed digest:

```sh
kubectl -n dibs set image deploy/dibs dibs=ghcr.io/hivecommons/dibs@sha256:30db26fa8f5c7f870e61892d81f6abe4b3b2ecdb2a455a51f3fe0eedc8995f68
kubectl -n dibs rollout status deploy/dibs --timeout=120s
kubectl -n dibs get pods -l app=dibs
curl -fsS https://dibs.hivecommons.dev/healthz
curl -fsS https://dibs.hivecommons.dev/readyz
```

Stop on any failed command. Verify one Ready pod after termination of the old
pod, stable restarts, and the actual pod `imageID` matching the selected
platform manifest above. Both public endpoints must return HTTP 200 and
`status: ok`; `/healthz` must report the **full selected sha**
`d411cb967f802c0515327fe302de3c1de5fe8409`, not merely a successful status.
Repeat through the public ingress to exclude mixed old/new responses.
Spot-check existing ideas without creating or modifying public data; inspect
logs/metrics for sustained 5xx. The repository manifest already preserves
the same pin for future applies; do not apply unrelated secrets or PVC
manifests as part of this image-only change.

If rollout/readiness/version or data checks fail, do not declare success.
Use [release rollback](../runbooks/release-rollback.md) with the retained
previous immutable image, wait for readiness and verify its expected public
version. If storage is damaged, stop writers and follow the recovery runbook;
restoring the backup can lose post-backup writes and needs operator approval.
Record a follow-up manifest revert to the previous digest so a later apply
does not reintroduce a failed release.

## Required completion record on #558

- Operator and UTC execution time/context confirmation.
- Actual previous image/digest and expected previous health version.
- Backup reference and successful verification (no private data attached).
- Target sha/index digest and observed platform imageID.
- Rollout result, final replica/readiness state, public health version and
  readyz HTTP status, and post-rollout error/data spot checks.
- Rollback result if needed, or explicit successful acceptance.

Only that operator evidence completes #558. A merged handoff, green CI, or
an approved owner decision alone does not establish deployment.
