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

## Deployment

Render (native Go runtime, Frankfurt), auto-deploys after the CI checks pass.

- Build: `go build -tags netgo -ldflags '-s -w' -o app`
- Start: `./app`
- Health check path: `/livez`

CI (`.github/workflows/ci.yml`) runs on every push: formatting, `go vet`, tests with the race detector, and the build.
