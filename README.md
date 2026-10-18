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

- **429** — the account cools down (until `Retry-After`, else the
  subscription's `anthropic-ratelimit-unified-reset`, else 10 minutes) and
  the client is retried on the next account; it returns to the pool when the
  cooldown expires.
- **401** — the account is marked invalid and excluded until the token
  changes on reload.
- Transport errors — the account is benched briefly and another is tried
  (a client hanging up mid-request does not count against the account).

## Install

Prebuilt binaries for linux/amd64 are attached to every
[release](https://github.com/dehydr8/cproxy/releases) — a versionless
`cproxy-linux-amd64.tar.gz` keeps a stable latest-download URL:

```sh
curl -fsSL https://github.com/dehydr8/cproxy/releases/latest/download/cproxy-linux-amd64.tar.gz \
  | tar xz -C /tmp
install -m 755 /tmp/cproxy ~/.local/bin/cproxy
```

or with Go 1.27+:

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

## Wrapping the claude command (`cproxy install`)

`cproxy run` only covers launches you make yourself. `cproxy install` makes
**every** `claude` launch go through cproxy, whatever started it — your
shell, a script, or Amp's Claude Code mode:

```sh
cproxy install          # wrap claude; `cproxy uninstall` restores it
```

The real binary is moved to `claude.cproxy-real` beside itself and a tiny
shim takes its place. The shim holds no secrets and makes no decisions; it
just execs `cproxy claude --exec <real binary>`, which on every launch:

1. picks the client token claude will present: `$CPROXY_AUTH_TOKEN`, a
   matching `$ANTHROPIC_AUTH_TOKEN`, or the first proxy-scoped client token
   in `$CPROXY_CLIENT_TOKENS` / `credentials.json`. With none configured it
   creates a private local token (`~/.cproxy/local-token`, `0600`) shared
   with the serve it starts, so pool accounts alone are enough;
2. starts a shared `cproxy serve` on `127.0.0.1:7788` (`$CPROXY_PORT`) if
   nothing is listening there yet — inheriting the current environment, so
   env-configured credentials just work — logging to `~/.cproxy/serve.log`;
   then checks that whatever listens there is a cproxy holding that client
   token (a challenge that never sends the token itself), so the token is
   never handed to another program on the port and a stale serve is
   reported with the `kill <pid>` to fix it;
3. launches the real claude with `ANTHROPIC_BASE_URL` /
   `ANTHROPIC_AUTH_TOKEN` pointed at it (and `CPROXY_*` secrets stripped).

It backs off — running claude completely untouched — when there is nothing
to inject: no configured credentials, or an `ANTHROPIC_BASE_URL` /
`ANTHROPIC_AUTH_TOKEN` the environment already carries (whoever set them
wins). A configured client token with no pool accounts to serve it is an
error, surfaced loudly rather than silently routing wrong.

Claude Code's self-updater replaces its binary, which overwrites the shim —
re-run `cproxy install` after updates. Both `install` and `uninstall`
accept `--path` for a binary that is not on PATH.

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
| `cproxy install [--path P]` | replace the `claude` binary with the cproxy shim (undo: `cproxy uninstall`) |
| `cproxy claude [--port N] [-- cmd…]` | run the real claude through the shared local serve (what the shim execs) |
| `cproxy usage [--port N] [--json]` | print live usage of a running serve instance in the terminal |
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
replaces the `Authorization` header with a pool account's credential. Nothing
is ever written back: `credentials.json` (or the env) is the only token store;
`cproxy token` mints and prints one, you put it where you want it.

- Edits to `credentials.json` (e.g. `cproxy accounts add`) are picked up
  within ~2s, or immediately on `SIGHUP`, without dropping connections
  (accounts whose tokens changed lose their cooldowns and cached usage).
  Environment variables are read once at startup; restart serve to change
  them.
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

- **HTML** (auto-refreshes every 2s; no external scripts): accounts with request counts, 429s,
  cooldown state, sticky pin per client, real utilization from Anthropic's
  usage API, and the last-seen
  `anthropic-ratelimit-*` values from upstream — e.g.
  `blocked · req 0/50 · tok 38.0k/40.0k · reset req 2m0s/tok 32m0s`.
- **JSON**: `GET /usage?format=json` for scripts and alerting.
- **Fragment**: `GET /usage?fragment=1` returns just the stats tables.
- **Terminal**: `cproxy usage [--port N] [--json]` prints the live table in a
  shell, authenticating with a client token from the same places serve reads
  them (env or `credentials.json`); it needs the `usage` scope. Only a shared
  serve instance (serve, or the wrapper's spawned one) can be queried — a
  one-shot `cproxy run` session keeps its port and session token to itself.

Auth is a client token via the `Authorization` header, or — since browsers
can't set headers — `GET /usage?token=cp_…` (the token stays in the URL, so
prefer a dashboard-scoped token for this). Tokens without the `usage` scope
get `403 permission_error`.

Usage comes from two mechanisms, and the first is preferred whenever the
token can answer it:

1. **Real usage API** (preferred): cproxy polls
   `GET /api/oauth/usage` in the background — lazily, starting with the first
   usage view — and shows per-account 5-hour and 7-day utilization
   (`5h 42.5% (resets 1h30m) · 7d 12%`), spend with its monthly limit
   (`spend USD 25.5/500`) and per-model weekly limits (`Fable 20%`), as
   `usage_api` in the JSON. This needs the `user:profile` scope — which
   `cproxy setup-token` mints by default. Tokens that cannot query it (an
   inference-only token, a revoked one) show a note like
   `usage API: 403 (token needs the user:profile scope)` instead, and back off
   instead of retrying. API-key accounts are not polled.
2. **Rate-limit headers** (labeled fallback): the
   `anthropic-ratelimit-requests-*`, `anthropic-ratelimit-tokens-*` and
   `anthropic-ratelimit-unified-status` headers cproxy already sees as the
   proxy, which keep request/token granularity even when the usage API is
   unavailable.

Both are shown side by side (usage API numbers in the `usage` column, inferred
rate limits in `rate limits`), plus cproxy's own counters (requests/429s/errors/
sticky pins per account and client). Upstream tokens are always masked in
dashboard and JSON output.

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

Amp threads can run in **Claude Code mode**. The wiring lives entirely in
the `claude` wrapper — nothing is added to `.agents/setup`,
`.agents/resume`, or `.amp/services.yaml`, so the repository stays clean
and machines without cproxy behave exactly as before.

1. Install cproxy in the orb and wrap `claude` (the post-install script
   below does both).
2. In Amp's **Secrets & Env Vars** (personal or project), add
   `CPROXY_TOKENS` (or `CPROXY_POOL_TOKENS_<NAME>`) — the pooled account
   tokens, marked as secrets. That is all claude needs; the wrapper creates
   its own local client token. Optionally add `CPROXY_CLIENT_TOKENS`, e.g.
   `monitor:cp_…:usage` for a dashboard-only viewer.
3. The first `claude` launch in an orb starts serve on `127.0.0.1:7788`
   (inheriting those env vars), injects the routing into claude, and
   registers the `/usage` dashboard as an orb portal. Each Amp thread gets
   a stable client identity, sticks to one account, and fails over on 429.

If the vars are absent the shim injects nothing, so the same setup works
with or without cproxy. Clients that never launch the `claude` binary point
at a shared `cproxy serve` with `ANTHROPIC_BASE_URL` / `ANTHROPIC_AUTH_TOKEN`
instead (the wrapper sees those and stays out of the way).

### Amp post-install script

Set this as Claude Code's **post-install script** (Claude Code Settings in
Amp; it runs after Amp installs Claude Code and before it launches). It
installs cproxy and wraps `claude`, only inside Amp orbs:

```sh
#!/bin/sh
set -eu
# Only inside Amp orbs; anywhere else leave claude untouched.
[ "${AMP_ORB:-}" = "1" ] || exit 0
export PATH="$HOME/.local/bin:$PATH"
if ! command -v cproxy >/dev/null 2>&1; then
  mkdir -p "$HOME/.local/bin"
  curl -fsSL https://github.com/dehydr8/cproxy/releases/latest/download/cproxy-linux-amd64.tar.gz \
    | tar xz -C /tmp
  install -m 755 /tmp/cproxy "$HOME/.local/bin/cproxy"
fi
cproxy install
```

It leaves no credentials behind — `cproxy install` only swaps in the shim;
credentials arrive per-thread through the project env vars above (or a
`credentials.json` under `$CPROXY_HOME`, which works too). Running it again
is harmless; it reinstalls the shim after Claude Code updates.

## Security notes

- Upstream account tokens never appear in child environments, client requests,
  logs, `/usage` output, or JSON.
- Client tokens are the only thing other users ever hold; revoke them by
  removing them from `credentials.json` (picked up automatically) or from
  the environment (restart serve).
- `credentials.json` is written with `0600` and contains raw tokens — protect
  the home directory accordingly.
- Serve beyond localhost only with TLS or a terminating proxy in front.

## Development

```sh
go build ./... && go vet ./... && go test ./...
```

Layout: `cmd/` (cobra commands), `internal/config` (file + env config),
`internal/pool` (strategies, cooldowns, stats), `internal/proxy` (handler,
failover, usage dashboard), `internal/launch` (child process launching),
`internal/wrap` (claude shim install + shared-serve bootstrap).
