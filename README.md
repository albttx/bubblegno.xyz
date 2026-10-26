# bubblegno.xyz

Bubble map for [gno.land](https://gno.land) — addresses & realms as bubbles, transfers/`MsgCall` as links. Inspired by [Bubblemaps](https://v2.bubblemaps.io/).

**Stack (v0):** Go watcher + HTTP API · Dgraph · Onbloc tx-indexer GraphQL (`gnoland-1`).

## Layout

```
cmd/bubblegno/          # single binary (API + watcher)
internal/
  config/               # env config
  onbloc/               # GraphQL client → indexer.onbloc.xyz
  graph/                # Dgraph client stub
  api/                  # HTTP /healthz /v0/graph /v0/node/{id} + static web/dist
deploy/dgraph/          # GraphQL schema + docker volume
web/                    # Vite + TS force-directed bubble map (force-graph)
```

## Quick start (local)

```bash
cd ~/go/src/github.com/albttx/bubblegno.xyz
cp .env.example .env
make up          # Dgraph zero + alpha
# wait a few seconds for alpha healthy
make schema      # apply deploy/dgraph/schema.graphql
make run         # API :8090 + watcher stub (Onbloc probe)
```

- API health: `curl localhost:8090/healthz`
- Dgraph Ratel / HTTP: `http://localhost:8080`
- Onbloc playground: https://indexer.onbloc.xyz/graphql

## Config

| Env | Default |
|---|---|
| `BUBBLEGNO_HTTP_ADDR` | `:8090` |
| `BUBBLEGNO_DGRAPH_HTTP` | `http://localhost:8080` |
| `BUBBLEGNO_ONBLOC_GRAPHQL` | `https://indexer.onbloc.xyz/graphql/query` |
| `BUBBLEGNO_CHAIN_ID` | `gnoland-1` |
| `BUBBLEGNO_START_HEIGHT` | `0` |

## Ingest

```bash
# dry-run a small window (no Dgraph)
BUBBLEGNO_START_HEIGHT=780 go run ./cmd/bubblegno -dry-run -once -window=50 -max-height=850

# with Dgraph up + schema applied
make up && make schema
BUBBLEGNO_START_HEIGHT=780 go run ./cmd/bubblegno -once -window=50 -max-height=850
```

Flags: `-window`, `-once`, `-dry-run`, `-max-height`, `-serve-only`.

Onbloc filter: `block_height: { gt, lt }` (no gte/lte) + `success: { eq: true }`. Cap ~10k rows/query.

## API

All JSON, CORS allowed for `http://localhost:*` / `127.0.0.1:*` / `bubblegno.xyz`.

- `GET /healthz` → `{ok, service, chain, dgraph: bool}`
- `GET /v0/graph?seed=&depth=1&limit=150`
  - `seed`: `g1…` address or realm `gno.land/r/…` (optional). Empty → busiest nodes from the latest ~3k transfers/calls/deploys.
  - `depth` 1–3 hops, `limit` 1–500 nodes.
  - → `{nodes:[{id,address,kind,pkgPath,label,txCount,volumeUgnot,degree,size}], links:[{source,target,weight,volumeUgnot,kinds}], seed, depth, limit, sync:{chainId,lastHeight}, warning?}`
  - Links are undirected and aggregated in Go from raw Transfer/Call/Deploy edges (no `Link` rows yet).
  - `size = clamp(4 + 2.5·log2(1+degree) + 3·log10(1+volumeUgnot/1e6), 4, 48)`
- `GET /v0/node/{id}` (id may contain `/`) → node fields + `neighbors[]`, `recentTransfers[]`, `recentCalls[]`, `deploys[]`, `sync`.
- `GET /` → SPA from `web/dist` (override with `BUBBLEGNO_WEB_DIR`).

## Frontend

```bash
cd web
npm i            # or: bun i
npm run dev      # http://localhost:5173 → API http://localhost:8090 (VITE_API_BASE to override)
npm run build    # web/dist, then served by bubblegno at http://localhost:8090/
```

## Run API + ingest

```bash
make up && make schema
make build && ./bin/bubblegno            # API :8090 + watcher (resumes from SyncState in Dgraph)
./bin/bubblegno -serve-only              # API only
BUBBLEGNO_HTTP_ADDR=:8091 ./bin/bubblegno -serve-only   # side-by-side API while ingest owns :8090
```

Restart is safe: watcher resumes from `SyncState.lastHeight`.

## Docker / deploy

- **Image:** `ghcr.io/albttx/bubblegno` — built and pushed by `.github/workflows/ghcr.yml` on every push to `main` (`:latest`, `:<sha>`) and on `v*` tags.
- **Local build:** `docker build -t bubblegno:local .`
- **Production:** `docker-compose.prod.yml` (Dgraph zero/alpha bound to `127.0.0.1` + `api` container on `:8090`, env from `/etc/bubblegno.env`):

```bash
docker compose -f docker-compose.prod.yml pull
docker compose -f docker-compose.prod.yml up -d
```

- **Front:** `web/` is deployed on Vercel (bubblegno.xyz) and talks to the API.

## Next

1. Ingest maintains `Link` aggregates + Account `txCount/volumeUgnot/degree` → API reads them directly
2. Token (GRC20) transfers, time-range filter, cluster highlighting

## Notes

- Onbloc returns `max elements per query reached (10000)` without tight filters — always bound `block_height`.
- No push/merge without explicit approval.
