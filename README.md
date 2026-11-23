# windiq-case

Small demo of a platform visualizing the output of a wind turbine.

The app fetches hourly met-mast data from the WindIQ API, computes AEP, P50 and P90 and shows them on a web dashboard.

## Running locally

Requires Go (see `go.mod`).

```sh
go test ./...
go run .
```

Then open http://localhost:8080.

## Endpoints

| Path | Purpose | Checked by |
|---|---|---|
| `/` | Dashboard | People |
| `/livez` | Process is alive, always `200` | Render health check |
| `/health` | Data is fresh: `200`, otherwise `503`, plus a JSON status | Uptime monitor (alerting) |

`/health` returns `503` when there is no data or when the newest measurement is older than `MAX_DATA_AGE` (default `3h`). Render deliberately checks only `/livez`: an outage of the WindIQ API must neither restart the app nor block deploys.

Every request except `/livez` is logged as one line (method, path, status, duration).

## Configuration

| Variable | Default | Meaning |
|---|---|---|
| `PORT` | `8080` | HTTP port (set by Render) |
| `MAX_DATA_AGE` | `3h` | Data age above which `/health` returns `503` |
| `WINDIQ_CLIENT_ID` | (required) | API client ID |
| `WINDIQ_CLIENT_SECRET` | (required) | API client secret |
| `WINDIQ_API_URL` | `https://windiq-mock-api.sliplane.app` | API base URL |
| `POLL_INTERVAL` | `5m` | Time between two polls |
| `SNAPSHOT_PATH` | `data/snapshot.json` | Snapshot file (Render: `/var/data/snapshot.json` on the persistent disk) |
| `MAX_INVALIDATIONS_PER_POLL` | `24` | A poll that would turn more valid hours invalid is rejected as a broken delivery |

## Ingest

- The first poll loads the complete history. After that only changes are fetched (`updated_since`), which covers new hours and corrections. `updated_since` is the newest `updated_at` already stored (the API's clock, not ours) minus one hour of overlap, so records that become visible late are not missed. The overlap only re-fetches a few records that change nothing.
- Per hour the highest `version` wins. Wind speeds arrive as number or string (decimal comma allowed), are converted to m/s and checked for plausibility (0 to 40 m/s). Slightly negative values down to -3 m/s are the sensor's zero offset in calm conditions and count as 0 m/s. Both limits come from the distribution of the API data: regular values stay below 25 m/s, followed by a gap up to 55 m/s and isolated spikes up to 83 m/s; negative values spread evenly from 0 to -2.89 m/s. About 5 % of the hours arrive in km/h and are converted. Placeholders like `-999` are invalid. Unusable values are kept as invalid hours and logged.
- Protection against broken deliveries: if one poll would turn more than `MAX_INVALIDATIONS_PER_POLL` valid hours invalid, nothing from it is taken over and the poll counts as failed. The store stays unchanged, so the next poll fetches the same records again and takes them over once the API has fixed them. If the problem persists, the data gets stale and `/health` alerts.
- Master data (mast, turbine, site) is reloaded on every poll. Implausible values (e.g. a loss given in percent) are rejected; the last valid version stays in use.
- After a failed poll the next try comes after 15 s, doubling up to the poll interval; `Retry-After` is honored.
- After every change the store is written atomically to the snapshot. On start the snapshot is loaded first, so the app shows data even when the API is down. A snapshot in an older format (after a change of the normalization rules) is still shown, but the first poll reloads everything and re-normalizes it; the old snapshot is only overwritten once that has succeeded.

## Deployment

Render (native Go runtime, Frankfurt), auto-deploys after the CI checks pass.

- Build: `go build -tags netgo -ldflags '-s -w' -o app`
- Start: `./app`
- Health check path: `/livez`

CI (`.github/workflows/ci.yml`) runs on every push: formatting, `go vet`, tests with the race detector, and the build.
