# windiq-case

Small demo of a platform visualizing the output of a wind turbine.

The app fetches hourly met-mast data from the WindIQ API, computes AEP, P50 and P90 and shows them on a web dashboard.

- Status page: https://stats.uptimerobot.com/xFEprpZ45p
- Operations: [RUNBOOK.md](RUNBOOK.md)

## Overview

One Go binary, one process:

```
WindIQ API ──(poll every 5 min)──> ingest ──> store (memory) ──> calculation (cached) ──> dashboard /
                                                 │                                          /health
                                                 └──> snapshot file (persistent disk)
```

| File | Content |
|---|---|
| `windiq.go` | API client: token, master data, measurements with cursor paging |
| `measurement.go` | Normalization and plausibility of wind speeds |
| `ingest.go` | Poll loop, retries, protection against broken deliveries, master data validation |
| `store.go` | In-memory data, versions per hour, caches |
| `snapshot.go` | Atomic snapshot on disk |
| `calc.go` | Yield calculation |
| `charts.go` | Server-side SVG charts |
| `server.go`, `limiter.go` | HTTP endpoints, request log, rate limit |

## Running locally

Requires Go (see `go.mod`) and API credentials.

```sh
go test ./...
WINDIQ_CLIENT_ID=... WINDIQ_CLIENT_SECRET=... go run .
```

Then open http://localhost:8080. The snapshot is written to `data/snapshot.json`.

## Endpoints

| Path | Purpose | Checked by |
|---|---|---|
| `/` | Dashboard; `?gaps=ignore` (default) or `?gaps=scale` | People |
| `/livez` | Process is alive, always `200` | Render health check |
| `/health` | Data is fresh: `200`, otherwise `503`, plus a JSON status | Uptime monitor (alerting) |

`/health` returns `503` when there is no data or when the newest measurement is older than `MAX_DATA_AGE` (default `3h`). One check thus covers three failures: the app is down, the ingest is broken, or the WindIQ API delivers nothing. The JSON also shows the last successful poll and the last ingest error. Render deliberately checks only `/livez`: an outage of the WindIQ API must neither restart the app nor block deploys.

Every request except `/livez` is logged as one line (method, path, status, duration).

## Configuration

| Variable | Default | Meaning |
|---|---|---|
| `PORT` | `8080` | HTTP port (set by Render) |
| `WINDIQ_CLIENT_ID` | (required) | API client ID |
| `WINDIQ_CLIENT_SECRET` | (required) | API client secret |
| `WINDIQ_API_URL` | `https://windiq-mock-api.sliplane.app` | API base URL |
| `POLL_INTERVAL` | `5m` | Time between two polls |
| `MAX_DATA_AGE` | `3h` | Data age above which `/health` returns `503` |
| `SNAPSHOT_PATH` | `data/snapshot.json` | Snapshot file (Render: `/var/data/snapshot.json` on the persistent disk) |
| `MAX_INVALIDATIONS_PER_POLL` | `24` | A poll that would turn more valid hours invalid is rejected as a broken delivery |
| `RATE_LIMIT_PER_SECOND` | `200` | Requests per second for all clients together; bursts up to twice that |

## Calculation

Over the 12 months up to the newest valid measurement (`calc.go`):

```
v_hub     = v_mast * (h_hub / h_mast) ^ alpha
AEP gross = sum over the valid hours of power(v_hub) * 1 h
AEP net   = AEP gross * (1 - availability) * (1 - electrical) * (1 - wake)
P50       = AEP net
P90       = P50 * (1 - 1.282 * U)
```

## Deployment

Render (native Go runtime, Frankfurt, Starter instance with a persistent disk), auto-deploys after the CI checks pass.

- Build: `go build -tags netgo -ldflags '-s -w' -o app`
- Start: `./app`
- Health check path: `/livez`
- Disk mounted at `/var/data`

CI (`.github/workflows/ci.yml`) runs on every push: formatting, `go vet`, tests with the race detector, and the build.

Monitoring: UptimeRobot checks `/health` and alerts by push and e-mail; see the status page above.

## Implementation notes

### What the API delivers (analysis of 8706 records, Nov 2026)

| Finding | Count | Handling |
|---|---|---|
| Hours in km/h instead of m/s, in blocks of one to four days | about 420 (5 %) | Converted. Taken as m/s, these hours would be 3.6 times too fast and P50 clearly too high. |
| `-999` placeholder | 40 | Invalid (gap) |
| `null` | 29 | Invalid (gap) |
| Slightly negative values, 0 to -2.89 m/s | 17 | Calm, 0 m/s |
| Isolated spikes 55 to 83 m/s | 13 | Invalid. Regular values stay below 25 m/s, then there is a gap up to 55 m/s; the limit of 40 m/s lies in that gap. |
| Hours without any record | about 70 | Gap |
| Records for an hour that already had one (newer versions) | 18 | Highest version wins |

### Decisions

| Decision | Why |
|---|---|
| One Go binary, no separate frontend | One deploy, nothing to keep in sync. Go templates and server-side SVG are enough for a dashboard. |
| No database, data in memory plus a snapshot file | The API holds exactly the year the calculation needs; about 8,800 rows fit easily in memory. The snapshot covers restarts while the API is down. A database (e.g. Postgres) would become worthwhile with many masts or a long history. |
| Render instead of GCP or Kubernetes | Simplest hosting that runs one process permanently, with a persistent disk, health check on deploy and automatic TLS. |
| `updated_since` from the API's own timestamps, with overlap | Our clock and the API's clock never get compared, so no record is skipped silently. |
| Reject rather than guess | Unknown units, implausible values and implausible master data are not taken over; the reason is logged. |
| Limits from the data | The plausibility limits (40 m/s, -3 m/s) were set after looking at the distribution of the real data. |

### Ingest

- The first poll loads the complete history. After that only changes are fetched (`updated_since`), which covers new hours and corrections. `updated_since` is the newest `updated_at` already stored (the API's clock, not ours) minus one hour of overlap, so records that become visible late are not missed. The overlap only re-fetches a few records that change nothing.
- Per hour the highest `version` wins. Wind speeds arrive as number or string (decimal comma allowed), are converted to m/s and checked for plausibility (0 to 40 m/s). Slightly negative values down to -3 m/s are the sensor's zero offset in calm conditions and count as 0 m/s. Placeholders like `-999` are invalid. Unusable values are kept as invalid hours and logged, grouped by reason.
- Protection against broken deliveries: if one poll would turn more than `MAX_INVALIDATIONS_PER_POLL` valid hours invalid, nothing from it is taken over and the poll counts as failed. The store stays unchanged, so the next poll fetches the same records again and takes them over once the API has fixed them. If the problem persists, the data gets stale and `/health` alerts.
- Master data (mast, turbine, site) is reloaded on every poll. Implausible values (e.g. a loss given in percent) are rejected; the last valid version stays in use.
- After a failed poll the next try comes after 15 s, doubling up to the poll interval; `Retry-After` is honored. The token is renewed before it expires and once more on a `401`.
- After every change the store is written atomically to the snapshot (temporary file, fsync, rename). On start the snapshot is loaded first, so the app shows data even when the API is down. A snapshot in an older format (after a change of the normalization rules) is still shown, but the first poll reloads everything and re-normalizes it; the old snapshot is only overwritten once that has succeeded.

### Load

- Requests never call the WindIQ API; they only read the store.
- The calculation and the charts are computed once per data change and cached; the newest measurement for `/health` likewise.
- Benchmarks with a full year of data (`go test -bench . -run '^$'`, laptop, 8 cores): the dashboard takes about 0.1 ms per request (about 15,000 requests per second in parallel), `/health` about 0.01 ms. On Render (0.5 CPU) bandwidth runs out before CPU.
- A global rate limit (`RATE_LIMIT_PER_SECOND`, default 200 per second) answers excess requests with `429` and `Retry-After`. It is global rather than per client IP because behind Render's proxy the client IP is only known from a forgeable header and may be shared. `/health` and `/livez` are never limited, so monitoring stays honest during a flood. Rejected requests are summarized in one log line per minute instead of one line each.
- The HTTP server has read, write and idle timeouts and a header limit against slow or hanging clients.

### Known limits

- One mast, one turbine. A second source would need its own client and a key per mast in the store.
- One instance only: the data lives in memory. Scaling out would need a shared database.
- Missing hours are either ignored or filled with the monthly mean. Interpolating short gaps between neighbouring hours would be more accurate but is not built.
- A value that is plausible but wrong (e.g. a block in km/h labeled as m/s) cannot be detected by any range check.
