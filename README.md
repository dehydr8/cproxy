# cproxy

`cproxy` is a proxy in front of the Anthropic API that controls the credential
sent upstream, so several Claude subscriptions can back a single Claude CLI.
Run it locally (`cproxy run -- claude`) or as a shared server (`cproxy serve`)
that multiple people point their Claude Code at.

It never touches Claude's own files (`~/.claude`, `.credentials.json`): the
only files it owns live under `~/.cproxy/` (or `$CPROXY_HOME`).

## Why sticky by default

A Claude conversation resends its full context on every request. If a proxy
rotated credentials per request, every account in the pool would pay for the
whole context. cproxy pins each client to **one** pool account and only moves
it when that account hits its limit (429) or dies (401/transport error), so a
conversation is paid for once, on one subscription. Other strategies
(`round-robin`, `least-used`, `random`) are available per pool.

Failover:

- **429** — the account cools down (honoring `Retry-After` when present) and
  the client is retried on the next account; it returns to the pool when the
  cooldown expires.
- **401/403** — the account is marked invalid and excluded until the token
  changes on reload.
- Transport errors — the account is benched briefly and another is tried.

## Install

Requires Go 1.24+:

```sh
go install github.com/dehydr8/cproxy@latest
```

or build from a checkout:

```sh
go build -o /usr/local/bin/cproxy .
```

## Quick start

```sh
# Mint a long-lived token with the account you want to pool (browser sign-in).
cproxy setup-token | cproxy accounts add cvp1 --pool cvp

# Launch Claude through the local proxy, drawing from the "cvp" pool.
cproxy run --pool cvp -- claude
```

`cproxy run` starts a local proxy, then launches the command with
`ANTHROPIC_BASE_URL=http://127.0.0.1:<port>` and a generated
`ANTHROPIC_AUTH_TOKEN` pointing at it. Ambient Anthropic credentials and
`CPROXY_*` secrets are stripped from the child's environment, so the selected
pool is the only thing the child can use. The child shares cproxy's process
group (Ctrl+C reaches it directly), and cproxy exits with the child's exit
code.

## Credentials

Accounts live in `~/.cproxy/credentials.json` (or `$CPROXY_HOME`), or entirely
in the environment:

```json
{
  "version": 1,
  "accounts": [
    {"name": "main",   "token": "sk-ant-oat01-…", "pools": []},
    {"name": "backup", "token": "sk-ant-oat01-…", "pools": []},
    {"name": "cvp1",   "token": "sk-ant-oat01-…", "pools": ["cvp"]}
  ],
  "pools": {
    "cvp": {"strategy": "sticky"}
  },
  "client_tokens": {
    "alice":   "cp_…",
    "monitor": {"token": "cp_…", "scopes": ["usage"]}
  }
}
```

Accounts with no `pools` belong to the built-in `default` pool. `strategy` is
one of `sticky` (default), `round-robin`, `least-used`, `random`.

### Environment configuration

Everything the file can say can also be said with env vars — useful in Amp
orbs, CI, or anywhere you don't want a credentials file:

| Variable | Meaning |
| --- | --- |
| `CPROXY_TOKENS` | accounts for the default pool: `name:token,name:token` |
| `CPROXY_POOL_TOKENS_<NAME>` | accounts for pool `<NAME>` (uppercased): `cvp1:tok,cvp2:tok` |
| `CPROXY_CLIENT_TOKENS` | serve-mode client tokens: `name:token[:scopes]`, scopes `+`-separated |
| `CPROXY_UPSTREAM` | upstream base URL (default `https://api.anthropic.com`) |
| `CPROXY_HOME` | config directory (default `~/.cproxy`) |

```sh
CPROXY_TOKENS=main:sk-ant-oat01-…,alt:sk-ant-oat01-… \
  cproxy run -- claude

CPROXY_POOL_TOKENS_CVP=cvp1:sk-…,cvp2:sk-… \
  cproxy run --pool cvp -- claude
```

Env entries override same-named file entries; flags override both.

## Commands

| Command | What it does |
| --- | --- |
| `cproxy run [--pool P] [-- cmd…]` | start a local proxy and launch `cmd` (default `claude`) through it |
| `cproxy serve [--addr :7788]` | run as a shared server with client-token auth |
| `cproxy setup-token` | mint a long-lived Anthropic OAuth token via browser sign-in |
| `cproxy accounts add/disable/enable/remove/list` | manage pool accounts in `credentials.json` |
| `cproxy pools` | list pools, strategies and members |
| `cproxy token [--scopes …]` | generate a random client token (`cp_…`) |
| `cproxy version` | print the version |

## serve mode

`cproxy serve` is for sharing one pool of subscriptions with several people
(or machines). Each user sets:

```sh
export ANTHROPIC_BASE_URL=http://your-host:7788
export ANTHROPIC_AUTH_TOKEN=cp_…    # their cproxy client token
```

Client tokens are registered with `--client-tokens alice:cp_…`,
`CPROXY_CLIENT_TOKENS=alice:cp_…,monitor:cp_…:usage`, or the
`client_tokens` map in `credentials.json`, and never travel upstream — cproxy
replaces the `Authorization` header with a pool account's credential.

- Send `SIGHUP` to reload `credentials.json` without dropping connections
  (accounts whose tokens changed lose their sticky assignments and cooldowns).
- Each client token can route to its own pool: `{"token": "cp_…", "pool": "cvp"}`.
- Run behind TLS (`--tls-cert/--tls-key`) or a terminating proxy when exposing
  beyond localhost. `--no-auth` exists for localhost-only experiments.

### Client token scopes

Scopes limit what a client token may do:

- `proxy` — send requests through cproxy (this is the normal use)
- `usage` — view the `/usage` dashboard and JSON

A token with no scopes may do both; `{"token": "cp_…", "scopes": ["usage"]}`
(or `CPROXY_CLIENT_TOKENS=monitor:cp_…:usage`) is a dashboard-only token for
someone who should see pool health but never spend it.

## Usage dashboard

Serve mode exposes `/usage`:

- **HTML** (htmx, auto-refreshes every 2s): accounts with request counts, 429s,
  cooldown state, sticky pin per client, and the last-seen
  `anthropic-ratelimit-*` values from upstream — e.g.
  `blocked · req 0/50 · tok 38.0k/40.0k · reset req 2m0s/tok 32m0s`.
- **JSON**: `GET /usage?format=json` for scripts and alerting.
- **Fragment**: `GET /usage?fragment=1` returns just the stats tables.

Auth is a client token via the `Authorization` header, or — since browsers
can't set headers — `GET /usage?token=cp_…` (the token stays in the URL, so
prefer a dashboard-scoped token for this). Tokens without the `usage` scope
get `403 permission_error`.

Rate limits come from two places: response headers cproxy already sees as the
proxy (`anthropic-ratelimit-requests-*`, `anthropic-ratelimit-tokens-*`,
`anthropic-ratelimit-unified-status`), and cproxy's own counters
(requests/429s/errors/sticky pins per account and client). Upstream tokens are
always masked in dashboard and JSON output.

## setup-token

`cproxy setup-token` runs the same browser OAuth (PKCE) flow as
`claude setup-token`: it prints a URL, you sign in with the account you want
to pool and approve, then paste back the `code#state` shown after approval. The
minted token prints on stdout; diagnostics print on stderr; nothing is written
automatically and no Claude files are touched. Defaults: scopes
`user:inference user:profile`, one-year lifetime (both overridable).

```sh
cproxy setup-token | cproxy accounts add cvp2 --pool cvp
cproxy setup-token --scopes user:inference --expires-in 7776000
```

Tokens minted this way are long-lived OAuth tokens like Claude Code's own —
treat them like passwords.

## Using it inside Amp

Amp threads can run in **Claude Code mode**, and Claude Code decides where to
send requests from `ANTHROPIC_BASE_URL` / `ANTHROPIC_AUTH_TOKEN`. cproxy fits
that seam, and can be configured entirely through env vars so no credentials
file is needed:

1. Run `cproxy serve` somewhere the orb can reach (your own host, or inside
   the orb itself via `.agents/setup` / `.amp/services.yaml`).
2. In the Amp project's **Settings → Secrets & Env Vars**, add:
   - `ANTHROPIC_BASE_URL` → your cproxy serve URL
   - `ANTHROPIC_AUTH_TOKEN` → a cproxy client token
   - plus the env config for cproxy itself if it runs in the orb
     (`CPROXY_TOKENS` / `CPROXY_POOL_TOKENS_<NAME>` / `CPROXY_CLIENT_TOKENS`)
3. Claude Code in the orb then routes through cproxy: each Amp thread gets a
   stable client identity, sticks to one account, and fails over on 429.

Keep the tokens marked as secrets in Amp so they stay hidden; give
dashboard-only viewers a `usage`-scoped client token.

## Security notes

- Upstream account tokens never appear in child environments, client requests,
  logs, `/usage` output, or JSON.
- Client tokens are the only thing other users ever hold; revoke them by
  removing them from the file/env and sending `SIGHUP`.
- `credentials.json` is written with `0600` and contains raw tokens — protect
  the home directory accordingly.
- Serve beyond localhost only with TLS or a terminating proxy in front.

## Development

```sh
go build ./... && go vet ./... && go test ./...
```

Layout: `cmd/` (cobra commands), `internal/config` (file + env config),
`internal/pool` (strategies, cooldowns, stats), `internal/proxy` (handler,
failover, usage dashboard), `internal/launch` (child process launching).
