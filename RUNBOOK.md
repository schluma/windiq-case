# Runbook

How to keep an eye on the app and what to do when something goes wrong.

## Where to look

| What | Where |
|---|---|
| Public status page (uptime of `/health`) | https://stats.uptimerobot.com/xFEprpZ45p |
| Data freshness and last ingest error | `GET /health` (JSON), and the status box at the top of the dashboard |
| Logs | Render dashboard, service, Logs |
| Deploys and rollback | Render dashboard, service, Events |
| CI | GitHub, Actions |

Alerts: UptimeRobot checks `/health` and alerts by push and e-mail when it returns `503` (no data, or the newest measurement is older than `MAX_DATA_AGE`, default 3 h) or does not answer at all.

## Regular check

1. Status page green, no incidents since last check.
2. `/health`: `"status":"ok"`, `data_age_seconds` below about 2 h, no `last_error`.
3. Render log: the last `yield` line. P50 should only move slightly between checks; a jump comes with a `master data updated` line (new loss or curve version) or with `corrected measurements`.
4. Warnings since the last check: search the log for `level=WARN` and `level=ERROR`.

## Log lines and what to do

| Log line | Meaning | Action |
|---|---|---|
| `poll failed ... api status 5xx` or a connection error | The WindIQ API is down or slow. | Nothing. The app retries after 15 s, doubling up to 5 min, and keeps showing the last data. If it lasts longer than `MAX_DATA_AGE`, the uptime alert fires; then report the API outage. |
| `poll failed ... api status 429` | The API throttles us. | Nothing, `Retry-After` is honored. If it keeps happening, raise `POLL_INTERVAL`. |
| `poll failed ... request token: api status 401` | The credentials were rejected (rotated or revoked). | Set the new `WINDIQ_CLIENT_ID` / `WINDIQ_CLIENT_SECRET` in Render, Environment. Saving restarts the service. |
| `rejected N fetched records: they would turn X valid hours invalid` | A delivery would invalidate more valid hours than `MAX_INVALIDATIONS_PER_POLL` (default 24). Nothing from it was taken over; the next poll fetches it again. | Look at the raw data of the affected hours via the API (`GET /v1/measurements?updated_since=...`). If it is a broken delivery: wait until the API fixes it, report it. If the invalidation is legitimate (e.g. a known sensor fault): raise `MAX_INVALIDATIONS_PER_POLL` for one poll, then set it back. |
| `invalid site, keeping last valid: ...` (or mast, turbine) | Implausible master data, e.g. a loss in percent instead of a fraction. The last valid version stays in use; the dashboard shows which version. | Report it to the API owner. Nothing to do in the app. |
| `invalid measurements reason=...` | Single unusable values (null, -999, out of range). They count as gaps. | Normal in small numbers. Many at once: check the raw data. |
| `corrected measurements count=N` | The API sent new versions of known hours. | Normal. `valid_to_invalid` shows how many removed data. |
| `save snapshot: ...` | The snapshot could not be written (disk full or missing). | Check the disk in Render. The app keeps working from memory; only a restart would need the API again. |
| `snapshot unusable, starting empty` | The snapshot file is broken. | Nothing, the first poll reloads everything. |
| `rate limit exceeded rejected_since_last_report=N` | More than `RATE_LIMIT_PER_SECOND` requests per second (default 200); excess requests get `429`. `/health` and `/livez` are never limited. | Check whether it is a load test or an attack. Raise the limit if legitimate traffic is affected. |
| `fatal ... WINDIQ_CLIENT_ID and WINDIQ_CLIENT_SECRET must be set` | Missing secrets; the new deploy does not start and the old one keeps running. | Set the secrets in Render. |

## Deploys

- Every push to `main` runs CI (format, vet, tests with race detector, build). Render deploys only after CI is green, and switches to the new version only when `/livez` answers.
- Rollback: Render, Events, pick the last good deploy, Rollback.
- Each deploy restarts the app: it loads the snapshot from the disk (`/var/data/snapshot.json`) and continues with an incremental poll. A few seconds of downtime during the switch are expected (persistent disk).

## Force a full reload

Needed after a change of the normalization rules (this happens automatically when `snapshotFormatVersion` in `snapshot.go` is raised) or if the data looks wrong:

- Preferred: raise `snapshotFormatVersion` and deploy. The old snapshot stays in use until the reload has succeeded.
- Alternative: Render, Shell: `rm /var/data/snapshot.json`, then restart. Until the first poll succeeds, the dashboard has no data.

## Configuration

See the table in the README. All settings are environment variables in Render; saving them restarts the service.
