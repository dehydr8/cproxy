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
whole context. cproxy pins each client to **one** pool account and keeps it
there, so a conversation is paid for once, on one subscription. Other
strategies (`round-robin`, `least-used`, `random`) are available per pool.

The pin moves **before** the account runs out, the way claude-swap's
auto-switch does: once the account's binding window (the higher of its
5-hour and 7-day utilization, read from every response's
`anthropic-ratelimit-unified-*` headers and from the usage API) reaches
**90%**, the client moves to the account with the most headroom, provided that
one is under 90% and at least 10 points lower, at most once per 5 minutes.
New clients start on the account with the most headroom. Set
`CPROXY_SWITCH_AT=80` for another threshold, or `0` to only move on limits
and errors.

Failover:

- **429** — the client is retried on the next account, and the account cools
  down according to what the 429 says:
  - the subscription is exhausted (`anthropic-ratelimit-unified-status:
    rejected`): until `anthropic-ratelimit-unified-reset`;
  - any other rate limit: `Retry-After`, else 30 seconds;
  - a bare 429 with no rate-limit headers at all: not at all. OAuth tokens
    get that for requests Anthropic refuses to serve on them (for example
    without Claude Code's system prompt), and the account keeps working.
- **401** — the account is marked invalid and excluded until the token
  changes on reload.
- Transport errors — the account is benched briefly and another is tried
  (a client hanging up mid-request does not count against the account).

## Install

Prebuilt binaries for Linux and macOS (amd64 and arm64) are attached to
every [release](https://github.com/dehydr8/cproxy/releases); versionless
`cproxy-<os>-<arch>.tar.gz` names keep stable latest-download URLs:

```sh
os=$(uname -s | tr A-Z a-z); arch=$(uname -m | sed 's/x86_64/amd64/;s/aarch64/arm64/')
curl -fsSL "https://github.com/dehydr8/cproxy/releases/latest/download/cproxy-$os-$arch.tar.gz" \
  | tar xz -C /tmp
install -m 755 /tmp/cproxy ~/.local/bin/cproxy
```

or with Go 1.27+:

```sh
go install github.com/dehydr8/cproxy@latest
```

Either way, `cproxy update` later moves it to the newest release.

or build from a checkout:

```sh
go build -o /usr/local/bin/cproxy .
```

## Two ways to run it

- **Local**: the pool tokens live on the machine running Claude. One
  per-user **daemon** serves every Claude on the machine — launched with
  `cproxy run`, through the installed `claude` shim, from Paseo profiles or
  scripts — so they share one set of sticky pins, cooldowns and usage, and
  one dashboard. See [The local daemon](#the-local-daemon).
- **Remote**: one `cproxy serve` holds the pool tokens and several people or
  machines use it. Clients only need its URL and a client token; see
  [Hosting a shared cproxy](#hosting-a-shared-cproxy).

## Quick start

```sh
# Mint a long-lived token with the account you want to pool (browser sign-in).
cproxy setup-token | cproxy accounts add cvp1 --pool cvp

# Launch Claude through the local daemon, drawing from the "cvp" pool.
cproxy run --pool cvp

# See how the pool is doing, in the terminal or the browser.
cproxy usage
cproxy dashboard
```

`cproxy run` starts the local daemon on first use (later launches reuse it),
then launches the command — `claude` by default — with `ANTHROPIC_BASE_URL`
and `ANTHROPIC_AUTH_TOKEN` pointed at it. Arguments after `--` that start
with a flag go to claude, so `cproxy run --pool cvp -- --resume x` works
like cswap's run. Ambient Anthropic credentials and `CPROXY_*` secrets are
stripped from the child's environment, and `CLAUDE_CODE_PROMPT_CACHE_TTL=1h`
is set unless you set it yourself (see [Prompt cache TTL](#prompt-cache-ttl)).
`CLAUDE_CODE_ENTRYPOINT` is left as the environment has it — stock behaviour
— unless `CPROXY_ENTRYPOINT` opts the launch into the custom-label workaround
for the faster-draining `sdk-*` rate (see
[The 5-hour meter and the entrypoint label](#the-5-hour-meter-and-the-entrypoint-label)).
The child shares cproxy's process
group (Ctrl+C reaches it directly), and cproxy exits with the child's exit
code. `--isolated` runs a private proxy for one launch instead.

Paseo (or any launcher) profiles become one line each:

```json
"claude-cvp": {
  "extends": "claude",
  "label": "Claude (CVP)",
  "command": ["/home/me/.local/bin/cproxy", "run", "--pool", "cvp", "--"]
}
```

## The local daemon

There is one daemon per user, started by the first `cproxy run`,
`cproxy claude` (the shim) or `cproxy dashboard`, and shared by every later
launch — like flyctl's agent:

- **One instance**, guaranteed by an exclusive lock (`~/.cproxy/daemon.lock`);
  concurrent first launches race safely and end up on the same daemon.
- **Claude talks to it on `127.0.0.1`** (`$CPROXY_PORT`, else 7788, else any
  free port when 7788 is taken, e.g. by another user).
- **cproxy talks to it over a private unix socket** (`~/.cproxy/cproxy.sock`,
  `0600`): `cproxy usage` and `cproxy dashboard` need no token or port.
- **Configuration follows the launches.** Every launch hands the daemon its
  `CPROXY_*` variables; when they differ from what the daemon runs with, it
  reloads in place (the last launch wins). Edits to `credentials.json` are
  picked up within ~2s.
- **Upgrades hand over without dropping anything.** A launch from a newer
  cproxy release asks the running daemon to hand over: it stops listening at
  once, a fresh daemon takes the port, and the old one finishes the streams
  it was serving.
- **Identities.** Every launch on the default pool shares one identity
  (`claude-local`) and so one sticky account; `cproxy run --pool cvp`
  launches share `claude-local@cvp`.

It logs to `~/.cproxy/daemon.log`; `cproxy daemon` runs it in the foreground.

## Wrapping the claude command (`cproxy install`)

`cproxy run` only covers launches you make yourself. `cproxy install` makes
**every** `claude` launch go through cproxy, whatever started it — your
shell, an editor, or a script:

```sh
cproxy install          # wrap claude; `cproxy uninstall` restores it
```

The real binary is moved to `claude.cproxy-real` beside itself and a tiny
shim takes its place. The shim holds no secrets and makes no decisions; it
just execs `cproxy claude --exec <real binary>`, which on every launch:

1. picks the client token claude will present: `$CPROXY_AUTH_TOKEN`, a
   matching `$ANTHROPIC_AUTH_TOKEN`, or the first proxy-scoped client token
   in `$CPROXY_CLIENT_TOKEN_*` / `$CPROXY_CLIENT_TOKENS` / `credentials.json`.
   With none configured it uses the private local token
   (`~/.cproxy/local-token`, `0600`, created once), so pool accounts alone
   are enough;
2. makes sure [the local daemon](#the-local-daemon) runs and has this
   launch's configuration, then checks that the daemon's port really is a
   cproxy holding that token (a challenge that never sends the token
   itself);
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

Everything the file can say can also be said with env vars — useful in
CI, containers, or anywhere you don't want a credentials file:

| Variable | Meaning |
| --- | --- |
| `CPROXY_TOKEN_<NAME>` | one account `<name>` in the default pool |
| `CPROXY_POOL_TOKEN_<POOL>_<NAME>` | one account `<pool>-<name>` in pool `<pool>` |
| `CPROXY_POOL` | the pool to use when `--pool` is not given (default `default`) |
| `CPROXY_SWITCH_AT` | utilization % at which a sticky client moves to a fresher account (default 90; 0 = only on limits) |
| `CPROXY_TOKENS` | several default-pool accounts in one variable: `name:token,name:token` |
| `CPROXY_POOL_TOKENS_<POOL>` | several accounts for pool `<pool>`: `cvp1:tok,cvp2:tok` |
| `CPROXY_CLIENT_TOKEN_<NAME>` | one serve-mode client token for client `<name>`: `token[:scopes]` |
| `CPROXY_CLIENT_TOKENS` | several client tokens in one variable: `name:token[:scopes]`, scopes `+`-separated |
| `CPROXY_UPSTREAM` | upstream base URL (default `https://api.anthropic.com`) |
| `CPROXY_HOME` | config directory (default `~/.cproxy`) |
| `CPROXY_ENTRYPOINT` | custom `CLAUDE_CODE_ENTRYPOINT` label for the launch (unset, `off` or `0` leaves the variable untouched) |

One variable per account is the easiest to manage as secrets: adding,
rotating or removing an account touches only its own variable.

```sh
# accounts "abc" and "xyz" in the default pool
CPROXY_TOKEN_ABC=sk-ant-oat01-…
CPROXY_TOKEN_XYZ=sk-ant-oat01-…

# accounts "cvp-a" and "cvp-b" in pool "cvp", used because CPROXY_POOL says so
CPROXY_POOL_TOKEN_CVP_A=sk-ant-oat01-…
CPROXY_POOL_TOKEN_CVP_B=sk-ant-oat01-…
CPROXY_POOL=cvp
```

Names are lowercased. Pool account names carry their pool as a prefix,
because account names are global (cooldowns and sticky pins are keyed by
them), and pool names cannot contain `_`. The same token under several
variables becomes one account in all of their pools, so one subscription is
never tracked twice. Env entries override same-named file entries; flags
override both.

## Commands

| Command | What it does |
| --- | --- |
| `cproxy run [--pool P] [--isolated] [-- args…]` | launch claude (or a command) through the local daemon |
| `cproxy usage [--json] [--port N]` | live usage in the terminal: per-account bars, clients (the daemon, or a serve with `--port`) |
| `cproxy dashboard` | print a link to the live dashboard (view-only token included) |
| `cproxy install [--path P]` | replace the `claude` binary with the cproxy shim (undo: `cproxy uninstall`) |
| `cproxy claude [-- args…]` | run the real claude through the local daemon (what the shim execs) |
| `cproxy daemon` | run the per-user local daemon in the foreground (normally started for you) |
| `cproxy daemon status/start/stop/restart/logs [-f]` | control the background daemon; `restart` moves it to the installed cproxy and keeps its config and open streams |
| `cproxy serve [--addr :7788] [--usage-addr A]` | run as a shared server with client-token auth |
| `cproxy setup-token` | mint a long-lived Anthropic OAuth token via browser sign-in |
| `cproxy accounts add/disable/enable/remove/list` | manage pool accounts in `credentials.json` |
| `cproxy pools` | list pools, strategies and members |
| `cproxy token [--scopes …]` | generate a random client token (`cp_…`) |
| `cproxy update [--check]` | update cproxy in place to the latest release (checksum-verified; `go install` builds via `go install`) |
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

## Prompt cache TTL

Signed in to claude.ai, Claude Code writes its prompt cache with a 1-hour
TTL. With `ANTHROPIC_AUTH_TOKEN` set — which is how it talks to cproxy — it
assumes an API token and drops to 5 minutes, so a conversation idle for more
than 5 minutes re-pays its whole context. `cproxy run` and the shim set
`CLAUDE_CODE_PROMPT_CACHE_TTL=1h` for the main conversation to restore that;
set it yourself (`5m`, or `1h`) to override. Clients of a shared
`cproxy serve` set it in their own environment. Unlike a signed-in
subscriber, Claude Code then keeps 1h even after the account is past its plan
limits.

## The 5-hour meter and the entrypoint label

Anthropic's plan meters currently charge headless Claude Code — `claude -p`
and Agent SDK apps like T3 Code — about 1.7× to 2.9× more of the 5-hour
window per token than the same tokens from the interactive CLI, because those
sessions label their requests `sdk-cli`/`sdk-ts` and that label is what is
weighted ([anthropics/claude-code#97074](https://github.com/anthropics/claude-code/issues/97074),
[pingdotgg/t3code#24649](https://github.com/pingdotgg/t3code/issues/24649)).
Weekly limits are not affected. Every request carries the label, as
`cc_entrypoint` in its billing block and in the User-Agent, and it comes from
`CLAUDE_CODE_ENTRYPOINT` in Claude Code's environment.

cproxy can apply the same workaround: `cproxy run` and the shim launch claude
with `CLAUDE_CODE_ENTRYPOINT=$CPROXY_ENTRYPOINT` (any custom name, e.g.
`acme-agent`), so its requests carry a custom client label — the same
workaround T3 Code and Paseo ship — and skip the `sdk-*` weighting. The name
must be custom: headless claude rewrites the literal `cli` (the interactive
TUI's default) to `sdk-cli`, so `cli` and `sdk-*` values are treated like
unset. With the variable unset — or `off` or `0` — cproxy leaves
`CLAUDE_CODE_ENTRYPOINT` exactly as the environment has it, including
absent: stock claude behaviour. An interactive TUI launched through cproxy
needs no label either way; claude defaults it to `cli` itself.

Clients of a shared `cproxy serve` set it in their own environment, like the
prompt cache TTL:

```sh
export CLAUDE_CODE_ENTRYPOINT=acme-agent   # any custom name
```

This is a workaround for an Anthropic-side bug, not an Anthropic-confirmed
fix: it only changes how usage is attributed (custom labels are the same
mechanism official surfaces like the VS Code extension use). It is opt-in —
no launch is labeled unless `CPROXY_ENTRYPOINT` is set — so there is nothing
to undo once the metering is fixed upstream.

## Hosting a shared cproxy

Run one `cproxy serve` that holds the pool, and hand each person (or
machine) a client token. The pool tokens never leave the server.

### On the client

There is nothing to install. Point Claude Code at the server:

```sh
export ANTHROPIC_BASE_URL=https://my-cproxy.fly.dev
export ANTHROPIC_AUTH_TOKEN=cp_…     # the client token you were given
claude
```

Also set `export CLAUDE_CODE_PROMPT_CACHE_TTL=1h` to keep the 1-hour prompt
cache; see [Prompt cache TTL](#prompt-cache-ttl). And set
`export CLAUDE_CODE_ENTRYPOINT=acme-agent` (any custom name) so headless use
is not charged the `sdk-*` rate; see
[The 5-hour meter and the entrypoint label](#the-5-hour-meter-and-the-entrypoint-label).
This one has to be the environment variable, not the settings `env` block:
cproxy is not launching claude there, and claude applies that block only to
processes it spawns.

If the cproxy shim is installed on that machine it sees `ANTHROPIC_BASE_URL`
and stays out of the way.

### On the server

Generate one client token per person with `cproxy token`, then start serve
with the pool and client tokens in its environment:

```sh
CPROXY_TOKEN_MAIN=sk-ant-oat01-… CPROXY_TOKEN_ALT=sk-ant-oat01-… \
CPROXY_CLIENT_TOKEN_ALICE=cp_… CPROXY_CLIENT_TOKEN_BOB=cp_… \
CPROXY_CLIENT_TOKEN_MONITOR=cp_…:usage \
  cproxy serve --addr :7788 --usage-addr 127.0.0.1:7789
```

`--usage-addr` moves the dashboard to its own listener: the public `--addr`
serves only the API (and answers 404 for `/usage`), so bind the usage
address to loopback or a private network. Terminate TLS in front of
`--addr` (or use `--tls-cert/--tls-key`).

### Fly.io

[`deploy/fly`](deploy/fly) is a ready template: a small distroless image
built from this repository, the API on `https://<app>.fly.dev`, and the
dashboard on the app's private network only. From the repository root:

```sh
fly apps create my-cproxy             # then set `app` in deploy/fly/fly.toml
fly secrets set -a my-cproxy \
  CPROXY_TOKEN_MAIN=sk-ant-oat01-… CPROXY_TOKEN_ALT=sk-ant-oat01-… \
  CPROXY_CLIENT_TOKEN_ALICE=cp_… CPROXY_CLIENT_TOKEN_BOB=cp_… \
  CPROXY_CLIENT_TOKEN_MONITOR=cp_…:usage
fly deploy -c deploy/fly/fly.toml --ha=false
```

To look at the dashboard, open a private tunnel and use a usage-scoped
token:

```sh
fly proxy 7789 -a my-cproxy
open "http://localhost:7789/usage?token=cp_…"            # browser
CPROXY_CLIENT_TOKEN_MONITOR=cp_…:usage cproxy usage --port 7789    # terminal
```

Keep it to **one machine** (`--ha=false`, no autostop): sticky pins,
cooldowns and usage caches live in memory, so a second machine would split
them, and a restart (deploy, `fly secrets set`) starts them fresh.

## Usage dashboard

The local daemon and `cproxy serve` expose `/usage`:

- **Browser**: `cproxy dashboard` prints a link to the local daemon's page
  with a generated view-only token (`~/.cproxy/dashboard-token`) in it — no
  token to configure. Light and dark, refreshes every 2s without jumping, no
  external scripts, and laid out as cards on phones. It shows headline tiles
  (accounts, requests and how many are streaming now, tokens, 429s,
  clients), then each account with its email and plan, state, which clients
  are pinned to it, 5-hour / 7-day / per-model / extra-usage meters, requests
  and tokens, and each client with its pool, pinned account, tokens and
  whether it is streaming. Accounts without
  usage API access get their 5-hour / 7-day meters from the last response's
  `anthropic-ratelimit-unified-*` headers, or (API keys) a summary like
  `req 38/50 · tok 31.2k/40.0k · reset tok 26s`.
- **JSON**: `GET /usage?format=json` for scripts and alerting.
- **Fragment**: `GET /usage?fragment=1` returns just the stats tables.
- **Terminal**: `cproxy usage` prints each account as a block of bar meters
  (green / amber from 70% / red from 90% in a terminal) with resets, spend,
  requests and tokens, then the clients. It asks the local daemon over its
  socket, so no token is needed; with `--port` it asks a `cproxy serve` on
  that port instead, with a usage-capable client token from the usual
  places. `--json` prints the raw JSON.

Auth is a client token via the `Authorization` header, or — since browsers
can't set headers — `GET /usage?token=cp_…` (the token stays in the URL, so
prefer a dashboard-scoped token for this). Tokens without the `usage` scope
get `403 permission_error`.

Usage numbers come from three places, freshest first:

1. **Rate-limit headers** (live, free): every response from an OAuth account
   carries its subscription windows (`anthropic-ratelimit-unified-5h/7d-*`),
   so an account in use updates on every request. API keys report request
   and token limits instead (`req 38/50 · tok 31.2k/40.0k`).
2. **Usage API** (`GET /api/oauth/usage`, polled in the background while
   someone looks at the usage — see the cadence below): 5-hour and 7-day
   utilization with resets, extra-usage spend against its monthly limit, and
   per-model weekly limits (`usage_api` in the JSON). The same tokens answer
   `GET /api/oauth/profile`, so each account shows its email and plan
   (`Max 20x`, `Team · 5x`). Both need the `user:profile` scope, which
   `cproxy setup-token` mints by default.
3. **Haiku probe** (inference-only tokens): the usage API refuses tokens
   without `user:profile` (403). For those, when an account has had no
   traffic for 15 minutes, cproxy runs one minimal real Claude Code turn with
   the token — claude-swap's method: `claude -p` on Haiku, no tools,
   settings, MCP or history, in a throwaway config directory, a few hundred
   tokens — and reads the 5-hour / 7-day windows from Claude Code's own
   `rate_limit_event`. At most every 15 minutes per account, only while the
   usage poller runs, and only where a `claude` binary is installed (so not
   on a remote serve). The dashboard marks these "via a Haiku probe".

Proactive switching uses the freshest of these for every account. Polling
cadence: the dashboard re-reads cproxy every 2s (no upstream cost); the usage
API is asked at most once a minute per account and the profile every 6
hours, both only while a dashboard or `cproxy usage` has been looked at in
the last 5 minutes, backing off on errors. Upstream tokens are always masked
in dashboard and JSON output.

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

## Amp

Earlier versions documented an Amp setup: a Claude Code **post-install
script** installed cproxy in each orb and ran `cproxy install`, so Amp's
Claude Code mode launched `claude` through the shim. Amp has since removed
that post-install hook, and its Claude Code mode now runs the Claude Agent
SDK directly instead of launching the `claude` binary, so there is no
`claude` for the shim to wrap. cproxy no longer has any Amp-specific
behavior (the orb portal for the dashboard is gone too); use the shim or
`cproxy run` wherever the `claude` CLI itself is launched.

## Security notes

- Upstream account tokens never appear in child environments, client requests,
  logs, `/usage` output, or JSON.
- Client tokens are the only thing other users ever hold; revoke them by
  removing them from `credentials.json` (picked up automatically) or from
  the environment (restart serve).
- The local daemon's control socket is `0600` in your `0700` `~/.cproxy`:
  only you can reach it, which is why it needs no token.
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
`internal/daemon` (the local daemon's lock, socket and control protocol),
`internal/wrap` (claude shim install, daemon bootstrap).
