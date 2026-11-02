# Runbook: rolling back a Dibs release

Use this when errors, restarts, or a failing `/readyz` began right after a
rollout. Roll back first, diagnose afterwards.

## How releases reach the cluster

The `docker.yml` workflow publishes `ghcr.io/hivecommons/dibs` to `latest` plus
the full commit sha after CI is green on `main`. `deploy/deployment.yaml`
uses a `RollingUpdate` with `maxSurge: 1` and `maxUnavailable: 0`, so the old
pod keeps serving until the new one is Ready. A release that never becomes
Ready therefore stalls instead of taking Dibs down; a release that becomes
Ready and then misbehaves needs the rollback below.

Release candidates are reviewed weekly and handed to an operator; see
[Weekly production image review](../deploy/README.md#weekly-production-image-review)
for the cadence and the handoff record (including the previous image/digest
to roll back to).

## 1. Confirm the rollout is the cause

```sh
kubectl -n dibs rollout status deploy/dibs --timeout=60s
kubectl -n dibs rollout history deploy/dibs
kubectl -n dibs get deploy/dibs -o jsonpath='{.spec.template.spec.containers[0].image}{"\n"}'
curl -fsS <base>/healthz   # reports the running `version`
```

Compare the start of the errors with the rollout time. If they do not line up,
return to [incident-response.md](incident-response.md).

## 2. Check for data changes first

If the release touched the store format (`ideas/`, `index.json`, notifications,
repo profiles), take a backup before rolling back, per
[data-recovery.md](data-recovery.md). Older code may not read newer files.

## 3. Roll back

Preferred: return to the previous ReplicaSet.

```sh
kubectl -n dibs rollout undo deploy/dibs
kubectl -n dibs rollout status deploy/dibs --timeout=120s
```

If the manifest uses `:latest`, `rollout undo` reverts only the pod template
and may pull the same moving tag again. Pin the last known-good commit sha
instead (find it from the previous `version` in `/healthz`, the release
history, or the green `main` commits):

```sh
kubectl -n dibs set image deploy/dibs dibs=ghcr.io/hivecommons/dibs:<good-sha>
kubectl -n dibs rollout status deploy/dibs --timeout=120s
```

Then make the pin durable: change `image:` in `deploy/deployment.yaml` to the
same sha in a PR, so the next `kubectl apply -f deploy/` does not undo it.

## 4. Verify recovery

- `kubectl -n dibs get pods -l app=dibs` shows one Ready pod with a stable
  restart count.
- `curl -fsS <base>/readyz` returns `ok`; `/healthz` reports the good version.
- The alerts that fired (see [incident-response.md](incident-response.md))
  resolve, and `metrics http` log lines show no sustained `status=5xx`.

## 5. Follow up

1. Open or update the tracking issue with the bad and good versions and times.
2. Hold further releases until a fix lands and passes CI; per
   [slo.md](slo.md), an exhausted error budget means release only fixes.
3. If data was written by the bad release, continue with
   [data-recovery.md](data-recovery.md).
4. Write a postmortem from [postmortem-template.md](postmortem-template.md)
   for any budget-consuming incident.
