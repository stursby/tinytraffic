# tinytraffic

Simple, privacy-friendly web analytics for personal use. One binary, one
DuckDB file, no cookies.

- **Tracker**: ~1 KB gzipped. Pageviews (including SPA navigation), outbound
  link clicks and custom events. Accepts Plausible's event format, so existing
  Plausible integrations work.
- **Privacy**: no cookies or storage. Visitors are identified by
  `hash(daily salt, site, IP, user agent)`. The salt rotates at midnight in the
  site's timezone, and the IP is never stored. Referrers keep their domain and path; query strings are
  dropped.
- **Dashboard**: visitors, visits, pageviews, bounce rate and visit duration
  over time, plus sources, referrers, pages, locations, devices and goals.
  Click any row to filter. Light and dark themes, works on phones. Make a
  site's stats public to share its dashboard with anyone, no sign-in needed
  (rate limited to 300 requests a minute per IP).
- **API**: API keys for scripts and other apps. Read stats, manage sites, and
  send events from your server. See [docs/api.md](docs/api.md).
- **Stack**: Go + DuckDB backend. The dashboard is TanStack Start (SPA mode)
  with Solid and shadcn-style components, embedded in the binary.

## Quick start

With Docker:

```sh
docker run -d --name tinytraffic -p 8080:8080 -v tinytraffic:/data \
  -e TT_PASSWORD=change-me ghcr.io/egoist/tinytraffic:main
```

Behind a reverse proxy (Traefik, Caddy, nginx or Dokploy), add
`-e TT_TRUST_PROXY=true` and drop `-p 8080:8080`, so the proxy is the only way
in. Visitors are then located by their real IP. Without the flag, every visitor
appears to come from the proxy and shows as "Unknown", and viewers of public
dashboards share one rate limit. In Dokploy, add
`TT_TRUST_PROXY=true` under the app's Environment tab.

Or from source. You need [Go](https://go.dev) 1.26+, [Bun](https://bun.sh)
1.4+ and a C compiler (DuckDB is a C++ library, so the Go build uses cgo):

```sh
bun install                   # also fetches the GeoIP database
bun run build                 # tracker + dashboard + bin/tinytraffic
./bin/tinytraffic             # http://localhost:8080
```

On first visit you choose a dashboard password, unless `TT_PASSWORD` set one.
Then add a site and paste its snippet into the site's `<head>`:

```html
<script defer data-domain="example.com" src="https://stats.example.com/js/script.js"></script>
```

## Tracking

Custom events show up under Goals:

```js
tinytraffic("Signup", { props: { plan: "pro" } });
```

Outbound link clicks are tracked as `Outbound Link: Click` automatically.

Script attributes:

| Attribute | |
|---|---|
| `data-domain` | Site to record to. Defaults to the current hostname without `www.` |
| `data-api` | Event endpoint. Defaults to the script's own origin |
| `data-hash` | Count `#/routes` as separate pages |
| `data-outbound="false"` | Don't track outbound link clicks |

If a site moves, change its domain in the site's settings. Stats are kept,
and the old domain keeps recording to the same site until you remove it, so
pages with the old snippet still count.

Visits from localhost and automated browsers are ignored. To exclude your own
visits, run `localStorage.tinytraffic_ignore = "true"` in the console on your
site.

To send events from a backend instead, see
[Sending events](docs/api.md#sending-events) in the API docs.

## Configuration

| Flag | Env | Default | |
|---|---|---|---|
| `-addr` | `TT_ADDR` | `:8080` | Listen address |
| `-data` | `TT_DATA_DIR` | `./data` | Holds `tinytraffic.duckdb` |
| `-memory-limit` | `TT_MEMORY_LIMIT` | `1GB` | DuckDB memory cap |
| `-clickhouse` | `TT_CLICKHOUSE_URL` | | Use ClickHouse instead of DuckDB, see [ClickHouse](#clickhouse) |
| `-geoip` | `TT_GEOIP_DB` | see below | Comma-separated `.mmdb` files (MaxMind or DB-IP format, country or city) |
| `-trust-proxy` | `TT_TRUST_PROXY=true` | off | Read client IPs from `X-Client-IP` / `CF-Connecting-IP` / `X-Real-IP` / `X-Forwarded-For`. Enable only behind a reverse proxy |
| | `TT_PASSWORD` | | Sets the password on first start, so nobody else can claim the setup screen |

### GeoIP

Locations come from DB-IP's free city database (CC BY 4.0), installed from npm
as [`@ip-location-db/dbip-city-mmdb`](https://www.npmjs.com/package/@ip-location-db/dbip-city-mmdb).
The Docker image includes it, and a source build finds it in `./node_modules`
when started from the repository. To use another database, e.g. MaxMind
GeoLite2, put it at `DATA/geo.mmdb` or pass `-geoip`. The Locations card shows
the credit line the database's license asks for.

To refresh it: `bun update @ip-location-db/dbip-city-mmdb`.

### ClickHouse

Everything can live in [ClickHouse](https://clickhouse.com) instead of the
DuckDB file: sites, settings, the password, API keys, salts and events. Use it
for more traffic than one DuckDB file handles comfortably, or for deploys
without downtime: several tinytraffic processes can use one ClickHouse
database at once, so a new container can start before the old one stops.

```sh
tinytraffic -clickhouse clickhouse://user:password@host:9000/tinytraffic
```

The URL is clickhouse-go's DSN: `clickhouse://` for the native protocol
(port 9000) or `http://` / `https://` (port 8123). Create the database first,
e.g. `CREATE DATABASE tinytraffic`; tinytraffic creates its tables in it on
start. With ClickHouse, the data directory and `-memory-limit` aren't used,
except to find `geo.mmdb`.

tinytraffic expects a single ClickHouse server, not a cluster: its tables are
plain `MergeTree` tables created without `ON CLUSTER`, so they aren't
replicated or sharded.

To move an existing install over, stop the server, copy everything, then
start it with the URL:

```sh
tinytraffic migrate-clickhouse -data ./data -clickhouse clickhouse://user:password@host:9000/tinytraffic
tinytraffic -clickhouse clickhouse://user:password@host:9000/tinytraffic
```

`migrate-clickhouse` copies sites, API keys, settings (so the password and
sign-ins keep working), today's salt and all events. It's safe to run again:
what ClickHouse already has is kept, and after an interruption it resumes
where it stopped without copying an event twice. It stops with an error if a
site ID means a different site in ClickHouse, which happens if the server was
started on the empty ClickHouse database and a site was added there first.
Nothing is removed from the DuckDB file, so you can switch back by dropping
the flag; changes made in ClickHouse in the meantime aren't copied back.

With Docker, run it with the image against the same volume (the image's data
directory is `/data`), then add the URL to the server container:

```sh
docker stop tinytraffic
docker run --rm -v tinytraffic:/data \
  -e TT_CLICKHOUSE_URL=clickhouse://user:password@clickhouse-host:9000/tinytraffic \
  ghcr.io/egoist/tinytraffic:main migrate-clickhouse
# then start the server with -e TT_CLICKHOUSE_URL=... added to its other flags
```

The ClickHouse host must be reachable from inside the container, so
`localhost` won't work. If ClickHouse runs in Docker, put both containers on
one network and use its container name. If it runs on the host, use
`host.docker.internal` (on Linux, also pass
`--add-host=host.docker.internal:host-gateway`). In Dokploy, add
`TT_CLICKHOUSE_URL` under the app's Environment tab. Once the server runs on
ClickHouse, the volume is no longer needed, unless you keep a custom
`geo.mmdb` in it.

While two processes overlap, each keeps its own visit sessions in memory, so
a visitor whose requests reach both in those seconds may be counted as two
visits. Sites, API keys and the password changed in one process show up in
the other within 10 seconds. That also means commands like `password` and
`seed` work while the server runs.

### Commands

With DuckDB, these need the server stopped, since DuckDB allows one process
per database file. With ClickHouse, pass `-clickhouse` or set
`TT_CLICKHOUSE_URL` so they use it:

```sh
tinytraffic password                    # set a new dashboard password
tinytraffic seed -days 90 demo.local    # fill a site with demo traffic (-reset replaces it)
tinytraffic migrate-clickhouse          # copy everything to ClickHouse (see above)
```

With Docker, run them against the same volume:
`docker run --rm -it -v tinytraffic:/data ghcr.io/egoist/tinytraffic:main password`.

## Deploying

Put tinytraffic behind a TLS-terminating proxy, e.g. Caddy:

```
stats.example.com {
  reverse_proxy localhost:8080
}
```

and start it with `TT_TRUST_PROXY=true`, so visitors aren't all seen as the
proxy's IP.

**Docker images** are published to GHCR by `.github/workflows/docker.yml`, for
amd64 and arm64. Pushes to `main` publish `:main`; version tags (`v1.2.3`)
publish `:1.2.3`, `:1.2` and `:latest`. Data lives in the `/data` volume. To
build the image yourself: `docker build -t tinytraffic .`.

**Dokploy and Docker Swarm**: the database file is locked by one process at a
time, so the new container can't open `/data` while the old one still runs.
Dokploy updates services start-first by default, which makes the new container
fail with a DuckDB lock error, yet Dokploy still reports the deploy as done. In
the app's Swarm settings, set the Update Config and Rollback Config order to
`stop-first`. Each deploy then has a short outage. With
[ClickHouse](#clickhouse) there is no lock, so keep the default start-first
order and deploys have no outage.

**Backups**: stop the server and copy the data directory, or export to Parquet
with the DuckDB CLI:
`duckdb data/tinytraffic.duckdb "EXPORT DATABASE 'backup' (FORMAT parquet)"`.
With ClickHouse, back up its database instead, e.g. with `BACKUP DATABASE`.

## Development

```sh
bun run demo         # optional: a demo site with 90 days of fake traffic
bun dev              # everything; open http://localhost:3000
bun run test         # Go tests
TT_TEST_CLICKHOUSE=clickhouse://localhost:9000 bun run test   # also against ClickHouse
bun run typecheck
```

`bun dev` runs three processes with prefixed output:

- the tracker, rebuilt on save
- the Go server on :8080, rebuilt and restarted when a `.go` file changes,
  using `./data`
- the Vite dashboard on :3000 with hot reload, proxying `/api` and `/js` to
  the Go server

Ctrl-C stops all three.

`bun run demo` (re)creates `demo.tinytraffic.local` in `./data`, replacing its
previous stats, so it's safe to run again. Pass a domain or options with
`bun run demo -- --days 365 --visitors 5000 my-demo.test`. Stop `bun dev`
first.

Layout:

```
cmd/tinytraffic      entrypoint and subcommands
internal/ingest      /api/event: bot filter, UA/geo/referrer parsing, visitor hash, sessions
internal/store       sites, API keys, settings, salts and events in DuckDB or ClickHouse; batched writer; migration
internal/stats       dashboard queries (DuckDB and ClickHouse SQL)
internal/server      HTTP routes: dashboard API, public API (/api/v1), auth, static files
internal/seed        demo traffic generator
web/                 embedded build output (dashboard + tracker)
apps/dashboard       TanStack Start + Solid SPA
packages/tracker     tracker script
scripts/             bun dev and bun run demo
docs/api.md          public API reference
```

## Performance and scaling

Events go into one append-only table. Ingest is buffered in memory and flushed
once a second with DuckDB's appender, so a traffic spike costs one bulk insert
per second. Session metrics (bounce rate, duration, entry and exit pages) are
derived from events at query time.

Measured on an Apple Silicon laptop with 25M events (about a year at ~30K
visitors a day), the database is 555 MB:

- 30-day dashboard queries take 10–100 ms.
- 12-month queries take 0.3–0.6 s.
- Entry/exit pages over 12 months take ~1 s.

Expect a small VPS to be a few times slower.

Time buckets are computed with integer math, using UTC offsets precomputed in
Go, instead of `date_trunc`/`timezone()`. DuckDB's ICU timezone functions were
about 20× slower than the scan itself. A test checks the results against them
across DST changes.

If you outgrow one machine, move to [ClickHouse](#clickhouse). Several
tinytraffic processes can share it, but each keeps visit sessions in memory,
so send a visitor's requests to the same process (e.g. sticky sessions by
IP) when running more than one for long.
