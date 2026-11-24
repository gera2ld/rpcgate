# rpcgate

One JSON-RPC gateway for all your projects, for EVM chains and Solana. It runs
as a container on your own server, on your internal network. You enter the
provider URLs and keys (Ankr and others) in its admin UI, and they never leave
the server. Each client (a service, a script, an AI coding assistant) gets
its own URL with a token:

```
http://<server>:8545/<token>/<chain>        e.g. https://rpc.example.com/Xk3…9q/bsc
```

Each token has a profile with a method allowlist, a batch cap and a daily
budget. rpcgate follows no redirects and scrubs upstream URLs from everything
it returns. You manage RPC URLs, profiles and tokens in a PocketBase admin
UI.

## One port

Everything is on one port, `RPCGATE_LISTEN` (default `127.0.0.1:8545`,
`0.0.0.0:8545` in the image):

| Path | What |
|---|---|
| `/<token>/<chain>` | The proxy, for clients |
| `/<token>/status` | A token's own status |
| `/_/` | PocketBase's admin UI |
| `/api/` | PocketBase's API |

Proxy requests never pass through PocketBase's middleware, so its checks
(such as rejecting unknown `Authorization` headers) don't affect RPC clients.
PocketBase's backup API (`/api/backups`) is switched off: a backup is a copy of
the database, provider keys included.

## Setup on the server

1. Build the image: on GitHub, run **Actions → Image → Run workflow**. It
   pushes `ghcr.io/gera2ld/rpcgate:latest` and a `sha-<commit>` tag, for
   amd64 and arm64. Nothing builds on push. The package is private like the
   repository, so log the server in once with a token that can read packages:
   `docker login ghcr.io`. To build on the server instead, uncomment `build: .`
   in the compose file and keep a checkout next to it.

2. Copy `compose.example.yaml` to `compose.yaml`. It needs no `.env`: the
   provider keys go in the admin UI, not the environment.

3. Start it, and create your admin account:

   ```bash
   docker compose up -d
   ```

   ```bash
   docker compose exec rpcgate /rpcgate superuser upsert you@example.com '<password>'
   ```

4. In the admin UI at `http://<server>:8545/_/`, open **urls** and add one
   record per chain: pick the **chain**, give it a **name** such as `ankr`,
   paste the URL with your key, tick **enabled**, and save. With Ankr:

   | chain | url |
   |---|---|
   | `bsc` | `https://rpc.ankr.com/bsc/<key>` |
   | `arbitrum` | `https://rpc.ankr.com/arbitrum/<key>` |
   | `base` | `https://rpc.ankr.com/base/<key>` |
   | `polygon` | `https://rpc.ankr.com/polygon/<key>` |
   | `solana` | `https://rpc.ankr.com/solana/<key>` |
   | `ankr` | `https://rpc.ankr.com/multichain/<key>` (the Advanced API) |

   The chains reload at once, without a restart.

5. Check the result: `docker compose logs rpcgate`. On startup it prints a
   table of the chains with their state (`ready`, `failed` with the reason,
   `off`) and the URL in use, by name; the saved URLs by name;
   and each profile's limits. It never prints a URL or a token. Later
   reloads log one line per chain, such as `chain bsc ready (ankr)`.

Every chain must pass a check before it is served: `eth_chainId` must match the
chain's expected ID (EVM), and `getGenesisHash` its expected genesis hash
(Solana). A URL for the wrong chain, or a bad key, shows up as `failed`.

The chains are a fixed list in
[`internal/gateway/chains.go`](internal/gateway/chains.go): `bsc`, `arbitrum`,
`base`, `polygon`, `solana` (mainnet) and `ankr` (Ankr's Advanced API). A
chain without an enabled URL is `off`. To add one, add a line there, run
`just schema`, and rebuild.

The first start creates a `read-only` profile allowing every chain, with a
budget of 200,000 requests per token per chain per day. It creates no URLs and
no tokens.

The image is Alpine, running as `nobody`, so a shell is there for debugging:
`docker compose exec rpcgate sh`.

To run it without Docker: `just run` (data goes to `./pb_data`).

## URLs

Each record in **urls** is one chain's RPC URL with its key, used exactly as
saved, plus a name such as `ankr` (logs and status show the name, never the
URL) and an **enabled** toggle. A chain uses its **first enabled URL**, oldest
first. Keep as many per chain as you like (a second key, your own node) and
switch by ticking **enabled**; nothing needs deleting.

The URL is **write-only**. Once saved, it is never returned by the API or
shown in the admin UI, to superusers too. When you edit a record, leave
**url** empty to keep the saved one, or type a new one to replace it.

### Ankr's Advanced API

The `ankr` chain proxies Ankr's Advanced API, at
`/<token>/ankr`. Give it Ankr's multichain URL. Its methods name their chains
in their params, for example:

```bash
curl http://<server>:8545/<token>/ankr -H 'content-type: application/json' \
  -d '{"jsonrpc":"2.0","id":1,"method":"ankr_getAccountBalance","params":{"blockchain":["bsc"],"walletAddress":"0x..."}}'
```

Its default methods are the read-only `ankr_*` queries (balances, prices,
token and NFT holders and transfers, blocks, logs, transactions). It has no
chain ID to check, so it is ready once it has a URL. The daily budget and
batch cap apply as on any chain; note that Ankr bills these calls
at a higher rate than plain node calls.

## Creating a token

In the admin UI, open **tokens** → **New record**:

- **name**: who uses it, such as `indexer`. Logs and usage show this name, never
  the token itself.
- **token**: leave it empty; PocketBase generates a 40-character token when
  you save.
- **enabled**: tick it.
- **profile**: pick one, such as `read-only`.

Save, copy the token, and give the client its URL:

```
RPC_URL=http://<server>:8545/<token>/bsc
```

Changes take effect at once, without a restart. Untick **enabled** or delete
the record to revoke a token. Replace its value to rotate it.

A client can check what it may use and how much it has used today:

```bash
curl http://<server>:8545/<token>/status
```

It lists only the chains that token can call right now (in its profile, with
an enabled URL that passed its check), each with the token's full URL for it,
plus its methods, limits and usage today. Nothing about other tokens.

## Collections

The admin UI is the access-control screen. Change **records** there, never
the **schema**: the schema is a snapshot embedded in the binary
(`internal/store/collections.json`) and applied in full on every start, so a
UI schema edit is overwritten, and a field missing from the snapshot is
deleted along with its data. To change the schema, edit
`internal/store/schema.go`, run `just schema`, and commit both files.

All the collections are superuser-only: none has an API rule.

**urls**: `chain` (a select of the fixed chains), `name`, `url`
(write-only), `enabled`. See [URLs](#urls).

**profiles**: `chains` (a multi-select), `methods` (a JSON array of method
patterns; see [Methods](#methods)), `daily_budget` (0 = unlimited),
`max_batch` (0 = 50). The budget applies per token per chain per UTC day,
and counts each item in a batch. Editing it keeps today's count: raise it and
the token gets the difference; lower it below the count and the token stops
until midnight UTC. To give a token a fresh start today, delete its rows for
today in **usage** and restart rpcgate.

**tokens**: `name`, `token`, `enabled`, `profile`.

**usage**: daily totals per token, chain and method (UTC days): `requests`
(forwarded; these spend the budget), `rejections`, `upstream_errors`. Methods a
token may not call, and names that don't look like a method, are counted as
`(other)`.

## Methods

A profile's **methods** is a JSON array of patterns, where `*` matches
anything:

- `["*"]`: every method.
- `["eth_*", "getProgramAccounts"]`: every `eth_` method, plus one Solana
  method.
- `[]` (empty): each family's read-only defaults, below. Write and broadcast
  methods are not among them.

A non-empty list replaces the defaults; it does not add to them.

- **EVM** defaults: `eth_chainId`, `net_version`, `eth_blockNumber`,
  `eth_getBlockByNumber`, `eth_getBlockByHash`, `eth_getCode`, `eth_call`,
  `eth_getLogs`, `eth_getBalance`, `eth_getStorageAt`,
  `eth_getTransactionCount`, `eth_getTransactionByHash`,
  `eth_getTransactionReceipt`.
- **Solana** defaults: `getSlot`, `getBlockHeight`, `getBlock`,
  `getTransaction`, `getSignaturesForAddress`, `getAccountInfo`,
  `getMultipleAccounts`, `getBalance`, `getTokenAccountsByOwner`,
  `getLatestBlockhash`, `getHealth`, `getVersion`, `getGenesisHash`.
  `getProgramAccounts` and `sendTransaction` are not among them.
- **Ankr Advanced API** defaults: the read-only `ankr_*` queries; see
  [Ankr's Advanced API](#ankrs-advanced-api).

WebSocket subscriptions aren't supported yet; an upgrade request gets a 501.
The routes leave room for them at the same `/<token>/<chain>` path.

### Errors

Rejections are JSON-RPC errors. A budget rejection is `-32005` containing
"quota" (HTTP 429), so a client that shrinks its `eth_getLogs` range on
errors backs off instead. An
unknown, disabled or revoked token always gets the same `-32001 unauthorized`
(HTTP 401), whatever the reason.

## How it runs

PocketBase stays off the request path. At startup rpcgate loads chains,
URLs, profiles and tokens into memory, and record hooks reload them when you save in
the admin UI. Usage is counted in memory and flushed to `usage` every 15
seconds and on shutdown. Today's
totals are read back at startup, so daily budgets survive restarts. A crash
loses at most 15 seconds of counts.

Other variables: `RPCGATE_DATA_DIR` (default `./pb_data`, `/pb_data` in the
image; keep it on a volume) and `RPCGATE_QUIET=1` (stop logging each request).

## Security model

- **Provider keys stay on the server.** The RPC URLs, keys included,
  are stored in PocketBase's database on the data volume, in plain text. The
  API and the admin UI never return them, to superusers too, so they are
  write-only once saved. They never appear in a proxy response, the status
  page, the startup table or a log line. Responses are scrubbed of each URL
  and the key-like parts of it, transport errors are reduced to generic
  messages, and redirects are never followed, so a redirect can't carry a key
  elsewhere.
- **The data volume and its backups hold the keys.** Anyone who can read
  `/pb_data`, a copy of it, or a PocketBase backup (Settings → Backups, which
  a superuser can download) can read the keys. Protect them like the keys
  themselves.
- **Tokens are safe to share with internal clients and LLMs.** A token is
  revocable at once, limited by its profile (chains, methods,
  daily budget and batch cap), and only works from your internal
  network. A leaked token can spend at most its profile's budget before you
  revoke it, and it reveals nothing about the provider keys. Paste gateway
  URLs into configuration, chats and coding assistants as you like. Give each
  client its own token, so you can revoke one without touching the others.
  Tokens are kept only as hashes in memory and are never logged.
- **The admin login guards everything.** The admin UI is on the same port
  as the proxy, so anyone who can reach the proxy can reach the login. It
  controls all access and can point a chain at any URL, so use a strong
  superuser password. It can't read a saved URL back, and the backup
  API is off, so a stolen login doesn't directly expose the keys.
- **The proxy has no TLS.** Tokens travel in plain HTTP on your internal
  network. Run it on a network you trust, or put a TLS reverse proxy in front.

## Development

```bash
just check          # gofmt, go vet, go test -race ./...
just build          # bin/rpcgate
just docker-build   # rpcgate:latest
```

PocketBase is a library here, pinned to `ganigeorgiev.com/pocketbase/pocketbase
v0.42.3` in `go.mod`, because its Go API changes between minor versions before
1.0. Check its changelog before you bump it.
