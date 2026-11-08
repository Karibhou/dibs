# Runbook: responding to a Dibs incident

Use this when users report errors or the service looks unhealthy and the cause
is not yet known. If a recent merge is the obvious cause, go straight to
[release-rollback.md](release-rollback.md).

## 1. Triage (first 5 minutes)

1. Confirm user impact: which routes fail, since when, for whom.
2. Check the pods: `kubectl -n dibs get pods -l app=dibs`.
3. Probe both endpoints (see `pkg/server/server.go`):
   - `curl -fsS <base>/healthz`: process is alive; reports `version`.
   - `curl -fsS <base>/readyz`: 503 means the store's data directory is not
     accessible and writable, so the pod is not serving traffic.
4. Read the logs: `kubectl -n dibs logs deploy/dibs --since=30m`.
   - `readyz store ping failed` lines carry the filesystem error in `err`.
   - `metrics http` JSON lines (`method`, `route`, `status`=5xx, `count`, `avg_ms`) are
     flushed periodically and show which route groups are failing or slow.

## 2. Decide

| Symptom | Likely cause | Action |
|---|---|---|
| `DibsDown` firing (no successful scrape) | no pods running, pods crash-looping, or the metrics port/ServiceMonitor is broken | `kubectl -n dibs get pods -l app=dibs`; if pods are Ready, check the `dibs` Service `metrics` port and the ServiceMonitor target. |
| `/readyz` 503, pod not Ready | data volume unavailable or read-only | Check the `dibs-data-rwx` PVC and mount (`kubectl -n dibs describe pod`, `kubectl -n dibs get pvc`). Do not delete the PVC. |
| Errors began right after a rollout | bad release | Follow [release-rollback.md](release-rollback.md). |
| Pod restarting | crash or OOM | `kubectl -n dibs describe pod` for last state; `kubectl -n dibs logs --previous`. |
| 4xx on authenticated routes only | auth or secret misconfiguration | Verify the `dibs` secret and configmap in `deploy/` match what is deployed. |

## 3. Communicate

Record in the tracking issue: start time, user impact, current hypothesis, and
each action taken with its time. Update it whenever the state changes.

## 4. Close out

- Verify `/healthz` and `/readyz` are `ok` and the 5xx `metrics http` log lines have
  stopped.
- If data was lost or corrupted, follow [data-recovery.md](data-recovery.md).
- Open a postmortem from [postmortem-template.md](postmortem-template.md) for
  any user-visible outage or data-affecting event.

## 5. Background job alerts

For `DibsBackgroundJobFailing` and `DibsBackgroundJobStale`. These jobs do not
affect `/readyz`, so the service can look healthy while they fail.

1. Identify the job from the alert's `job` label. "Stale" is per job:
   `registry_sync` runs every 5 minutes and alerts after 1h; `catalog_refresh`
   runs weekly and alerts after 8 days.
2. Find its errors: `kubectl -n dibs logs deploy/dibs --since=2h | grep -i '"job"'`.
3. Check counters: `dibs_background_job_runs_total{job="<job>",result="error"}`
   and `time() - dibs_background_job_last_success_timestamp_seconds{job="<job>"}`.
4. Typical causes are an upstream (hub, GitHub, LLM) outage or an expired
   credential in the `dibs` secret. If an upstream is down, wait for recovery
   and confirm the staleness gauge resets; otherwise fix the credential and
   restart with `kubectl -n dibs rollout restart deploy/dibs`.
5. If errors began right after a rollout, follow [release-rollback.md](release-rollback.md).

## 6. Match-engine LLM alert

For `DibsMatchLLMErrorRatio`. LLM failures do not affect `/readyz`; matching
falls back to the deterministic path, so the service looks healthy while match
quality degrades.

1. Identify the affected operation from the alert's `op` label.
2. Confirm the failure rate: `dibs_match_llm_calls_total{op="<op>",outcome="llm_error"}`
   against the total for the same `op`, or in logs:
   `kubectl -n dibs logs deploy/dibs --since=1h | grep '"metrics match_llm"'`.
3. Typical causes are a gateway outage or rate limiting, or an expired or wrong
   `DIBS_LLM_API_KEY`, `DIBS_LLM_BASE_URL` or `DIBS_LLM_MODEL` in the `dibs` secret.
4. If the upstream is down, wait for recovery and confirm the ratio drops. If a
   credential or model name is wrong, fix the secret and restart with
   `kubectl -n dibs rollout restart deploy/dibs`.
5. If errors began right after a rollout, follow [release-rollback.md](release-rollback.md).

## 7. Pod restart alert

For `DibsPodRestarting`. A container that restarts and recovers can leave
`/readyz` green at scrape time while requests in flight are dropped.

1. Find the last termination reason:
   `kubectl -n dibs describe pod -l app=dibs` (`Last State`, `Reason`, `Exit Code`).
2. `OOMKilled` means the 256Mi memory limit in `deploy/deployment.yaml` is too
   low for the current load; raise the limit and roll out.
3. Otherwise read the crash output: `kubectl -n dibs logs deploy/dibs --previous`.
4. If restarts began right after a rollout, follow [release-rollback.md](release-rollback.md).
