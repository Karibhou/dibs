# Service level objectives for Dibs

These SLIs are measurable from the cumulative counters on the internal
`/metrics` endpoint (see `deploy/README.md`), from readiness probe state in
Kubernetes, and from the periodic `metrics` JSON log lines emitted by
`pkg/server/metrics.go`. Alert rules live in
`deploy/monitoring/prometheusrule.yaml`; alerts should link back to
[incident-response.md](incident-response.md).

| SLI | Definition | Source | Objective (30 days) |
|---|---|---|---|
| Availability | Share of time at least one pod is Ready (`/readyz` 200) | Kubernetes pod Ready condition; `dibs_http_requests_total{route_group="readyz",status_class="5xx"}` | 99.5% |
| Request success | Share of requests not answered `5xx` | `dibs_http_requests_total` (`status_class="5xx"` over all), or `metrics http` log lines | 99.5% |
| Latency | Average `avg_ms` for API routes stays under 500 ms in each 5-minute flush | `metrics http` log lines, `avg_ms` | 99% of flushes |

Probe semantics: `/healthz` is liveness only (process is up). `/readyz`
checks that the store's data directory is accessible and writable, which is
required to serve traffic, so it is the signal for availability.

## Measuring from /metrics

Counters are cumulative since process start (never reset), so use `rate` or
`increase`. Request success ratio:

```
1 - sum(rate(dibs_http_requests_total{status_class="5xx"}[30d]))
      / sum(rate(dibs_http_requests_total[30d]))
```

Background jobs: `dibs_background_job_runs_total{job,result}` and
`time() - dibs_background_job_last_success_timestamp_seconds{job}` for
staleness. LLM outcomes: `dibs_match_llm_calls_total{op,outcome}`.

## Measuring from logs

Logs are JSON, one object per line. Request metrics have `msg` `metrics http`
with fields `method`, `route`, `status` (class, e.g. `5xx`), `count`, `avg_ms`:

```sh
kubectl -n dibs logs deploy/dibs --since=24h \
  | jq -c 'select(.msg == "metrics http" and .status == "5xx")'
```

Sum `count` by `status` class to compute the success ratio. Log flushes cover one 5-minute window;
the `/metrics` totals are not reset.

LLM outcome counters have `msg` `metrics match_llm` with `op`, `outcome`,
`count`.

Background jobs (`catalog_refresh`, `registry_sync`) flush `metrics job`
lines with `job`, `outcome` (`ok` or `error`) and `count` per window, plus a
`metrics job last success` line per job with `last_success` (RFC 3339). A job
failing repeatedly, or whose `last_success` is stale, shows up with:

```sh
kubectl -n dibs logs deploy/dibs --since=1h \
  | jq -c 'select(.msg == "metrics job" and .outcome == "error")'
```

## Storage headroom

Not alerted; checked by hand. The store lives on `dibs-data-rwx`, a
statically provisioned NFS volume (`dibs-fss-pv` in `deploy/pvc.yaml`, the
OCI File Storage export `/hive-hub/dibs`, shared with the hub). For an NFS
volume the kubelet fills `kubelet_volume_stats_*` from `statfs` on the mount
(the in-tree NFS plugin uses `volume.NewMetricsStatFS`), so those series
describe the whole FSS file system, not the Dibs subdirectory and not the
nominal, unenforced `10Gi`. OCI reports an FSS file system as 8 EiB unless the
mount target sets a Reported Size or the file system has a quota
([OCI docs](https://docs.oracle.com/en-us/iaas/Content/File/Concepts/FSutilization.htm#Using)).
An available/capacity ratio alert would therefore either never fire or fire
on the hub's usage.

`/readyz` is not a headroom signal either: its write probe creates an empty
file, which can succeed on a full volume, so the first symptom is usually 5xx
on write routes (`DibsHigh5xxRatio`).

- Check monthly, at each target review below, and before any release that
  migrates the store or bulk-imports data, using step 1 of
  [incident-response.md section 8](incident-response.md#8-data-volume-full).
- Record `du -sh /data` (Dibs' own usage) and the `df -h /data` line in the
  tracking issue, so growth between checks is visible.
- When `df` reports a real limit (a `Size` well below `8.0E`) and less than
  15% of it is available, ask the cluster operator to raise the quota before
  writes start failing.

If the cluster gains a per-export metric for this share (for example an FSS
quota metric scraped into Prometheus), replace this manual check with an
alert in `deploy/monitoring/prometheusrule.yaml` pointing at section 8.

## Error budget policy

- Budget at 99.5% over 30 days is about 3.6 hours of unavailability, or 0.5%
  of requests.
- Above 50% of the budget spent: prefer fixes and reliability work over new
  features until the window recovers.
- Budget exhausted: release only fixes; follow
  [release-rollback.md](release-rollback.md) for any suspect rollout.
- Any budget-consuming incident gets a postmortem from
  [postmortem-template.md](postmortem-template.md).

## Review

Revisit these targets after the first month of measured data. Targets may be
tightened freely; loosening one requires a written reason in the tracking issue.
