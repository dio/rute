# rute

`rute` contains a Go OAuth 2.0 Authorization Code + PKCE library and a small
loopback-only compatibility proxy backed by Codex and Claude OAuth sessions.

## Install

Tagged releases contain CGO-free `rute` binaries for macOS and Linux on
`amd64` and `arm64`. Download the archive for your machine from
[GitHub Releases](https://github.com/dio/rute/releases), extract it, and move
`rute` somewhere on your `PATH`:

```sh
tar -xzf rute_0.1.0_darwin_arm64.tar.gz
install -m 0755 rute /usr/local/bin/rute
rute version
```

Replace the version, OS, and architecture in the filename as needed. On a
single-user macOS setup where `/usr/local/bin` is not writable, use a directory
on your personal `PATH` instead.

Homebrew users can install and upgrade through
[`dio/homebrew-tap`](https://github.com/dio/homebrew-tap):

```sh
brew install dio/tap/rute
brew upgrade rute
```

To build from source into a directory on your personal `PATH`:

```sh
CGO_ENABLED=0 go build -o "$HOME/.local/bin/rute" ./cmd/proxy
```

### Files and locations

| What | macOS | Linux |
| --- | --- | --- |
| Binary from a release archive | `/usr/local/bin/rute`, or a directory on your `PATH` such as `~/.local/bin` | same |
| Binary from Homebrew | `$(brew --prefix)/bin/rute` | same |
| State directory | `~/Library/Application Support/rute/` | `$XDG_STATE_HOME/rute/`, default `~/.local/state/rute/` |
| Credential bundle (`-credential-store=file`) | `<state>/credentials.json` | same |
| Account manifest (`-credential-store=keyring`) | `<state>/accounts.json` | same |
| Refresh tokens (`-credential-store=keyring`) | Keychain services `rute/codex` and `rute/claude` | Secret Service, same service names |
| Credential audit log | `<credentials.json or accounts.json>.audit.jsonl` | same |
| Call counters for the dashboard and usage | `<state>/metrics.db` | same |
| Estimated prices (`-prices`) | `<state>/prices.json`, created by you | same |
| Routing saved from the dashboard | `<state>/routing.json` | same |
| Running server's URLs and process | `<state>/server.json`, with the lock `<state>/server.lock` | same |
| Background rute log | `<state>/rute.log` | same |
| Call records (`-metrics-file`) | the path you pass, for example `~/Library/Logs/rute/calls.jsonl` | the path you pass, for example `~/.local/state/rute/calls.jsonl` |
| Service definition | `~/Library/LaunchAgents/io.github.dio.rute.plist` | `~/.config/systemd/user/rute.service` |
| Service logs | `~/Library/Logs/rute/proxy.log` and `proxy.error.log` | `journalctl --user -u rute.service` |

rute creates the state directory with mode `0700` and its files with mode
`0600`. `-credential-file` overrides the credential bundle path. Each file next
to a credential file with a `.lock` suffix is a lock file used to serialize
writes; leave it in place.

`rute claude`, `rute codex`, and `rute admin` start rute in the background
when it is not running, and it stops when idle; see [Quick start](#quick-start).
`rute serve` runs it in the foreground and stops on Ctrl-C. The service
definitions in [Access from other devices with Tailscale](#access-from-other-devices-with-tailscale)
keep it running all the time on the fixed ports instead.

## Quick start

Run Claude Code or the Codex CLI through rute:

```sh
rute claude
rute codex
```

Every argument after `rute claude` or `rute codex` goes to the client, so
`rute codex --yolo` and `rute claude --resume` work. A leading `--` is
dropped, as in `rute codex -- --yolo`.

The command:

1. finds a running rute, or starts `rute serve` in the background on free
   loopback ports;
2. when the provider has no account yet, opens a browser to sign one in as
   `default`;
3. runs the client pointed at rute: `ANTHROPIC_BASE_URL` for Claude Code, and
   `-c` provider overrides for Codex, so `~/.codex/config.toml` is left
   unchanged; and
4. returns the client's exit code.

A background rute keeps running while any client started this way is open,
and stops three hours after the last one exits with no calls in between. It
logs to `rute.log` in the state directory. Only one rute runs per state
directory; the running server records its URLs and process ID in
`server.json` there.

`rute admin` manages the background rute:

| Command | Does |
| --- | --- |
| `rute admin` or `rute admin ui` | opens the [dashboard](#management-dashboard-and-api), starting rute when none is running |
| `rute admin status` | prints the version, process, proxy and dashboard URLs, idle timeout, and accounts |
| `rute admin reload` | restarts rute on the same ports, for example after upgrading the binary; open clients keep working |
| `rute admin stop` | stops rute; open clients lose their proxy until the next `rute claude` or `rute codex` |

The dashboard's **restart** button does the same as `rute admin reload`.

Routing changed on the dashboard is saved to `routing.json` in the state
directory, so the next background rute uses it.

## Local proxy

> [!IMPORTANT]
> Use the provider's own client with subscription accounts: Claude Code (CLI
> or app) for Claude, and the Codex CLI or app for Codex. Claude subscription
> accounts reject requests from other clients, including `curl`, OpenWorker,
> and the Chat Completions adapter, with a bare `429`. rute passes that error
> through without cooling down the account. Point Claude Code at rute with
> `ANTHROPIC_BASE_URL=http://127.0.0.1:20128`, and Codex with the provider
> configuration in [Codex CLI](#codex-cli).

Start the proxy:

```sh
rute
```

If no credentials are supplied, it prints an authorization URL and waits up to
five minutes for the loopback OAuth callback. The resulting refresh token is
stored in the native macOS Keychain or Linux Secret Service. Access and ID
tokens stay in memory and are refreshed automatically.

The command listens on `127.0.0.1:20128` and exposes:

- `POST /v1/responses`, streaming or non-streaming (Codex)
- `GET /v1/responses` with a WebSocket upgrade (Codex)
- `POST /v1/chat/completions`, streaming or non-streaming (Codex or Claude)
- `POST /v1/messages`, streaming or non-streaming (native Claude)
- `GET /v1/models`
- `GET /healthz`

No downstream API key is required or checked. For that reason, `-listen`
rejects wildcard and non-loopback addresses. This command is intended as a
single-user local compatibility layer, not a multi-user gateway or virtual-key
service.

### Access from other devices with Tailscale

Keep `rute` bound to loopback and let Tailscale Serve provide HTTPS access to
devices in your tailnet. First configure `rute` to stay running in the
background.

On macOS, create a LaunchAgent at
`~/Library/LaunchAgents/io.github.dio.rute.plist`. Replace
`/absolute/path/to/rute` with the output of `command -v rute`, and replace
`/Users/YOU` with your home directory:

```sh
mkdir -p "$HOME/Library/LaunchAgents" "$HOME/Library/Logs/rute"
```

```xml
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key><string>io.github.dio.rute</string>
  <key>ProgramArguments</key>
  <array>
    <string>/absolute/path/to/rute</string>
    <string>serve</string>
    <string>-listen=127.0.0.1:20128</string>
    <string>-credential-store=file</string>
  </array>
  <key>RunAtLoad</key><true/>
  <key>KeepAlive</key><true/>
  <key>StandardOutPath</key><string>/Users/YOU/Library/Logs/rute/proxy.log</string>
  <key>StandardErrorPath</key><string>/Users/YOU/Library/Logs/rute/proxy.error.log</string>
</dict>
</plist>
```

Load the agent:

```sh
launchctl bootstrap "gui/$(id -u)" "$HOME/Library/LaunchAgents/io.github.dio.rute.plist"
launchctl print "gui/$(id -u)/io.github.dio.rute"
```

On Linux with systemd, create `~/.config/systemd/user/rute.service`. Replace
`/absolute/path/to/rute` with the output of `command -v rute`:

```ini
[Unit]
Description=rute local compatibility proxy

[Service]
ExecStart=/absolute/path/to/rute serve -listen=127.0.0.1:20128 -credential-store=file
Restart=on-failure
RestartSec=5

[Install]
WantedBy=default.target
```

Enable and start the user service:

```sh
mkdir -p "$HOME/.config/systemd/user"
systemctl --user daemon-reload
systemctl --user enable --now rute.service
systemctl --user status rute.service
```

To keep a systemd user service running after logout and start it at boot,
enable lingering for your account with `sudo loginctl enable-linger "$USER"`.

With Tailscale installed and signed in on the host, configure Serve:

```sh
tailscale serve --bg --set-path=/rute http://127.0.0.1:20128
tailscale serve status
```

Tailscale prints the host's tailnet URL. The proxy is available there under
`/rute`, for example:

```text
https://<host>.<tailnet>.ts.net/rute
```

Serve forwards requests over the host's loopback interface; `rute` does not
listen on the network interface. This Serve configuration is tailnet-only, so
clients must be signed in to the same tailnet and allowed by its access
controls. Do not expose this unauthenticated single-user proxy publicly with
Tailscale Funnel. The service manager starts `rute`; Tailscale Serve only
forwards traffic to it. Check the LaunchAgent logs or
`journalctl --user -u rute.service`, along with `tailscale serve status`,
when troubleshooting.

### Credential storage and accounts

Native key storage is the default and never silently falls back to plaintext:

```sh
rute -credential-store=keyring
```

Linux desktop sessions need a working, unlocked Secret Service collection.
For headless systems, explicitly opt into a private JSON file or ephemeral
memory:

```sh
rute -credential-store=file
rute -credential-store=memory
```

The file backend uses `0600` files inside a `0700` state directory. Override
its location with `-credential-file=/absolute/path/credentials.json`.

Without `-credential-store`, rute uses the store that already has accounts:
the keyring when its account list (`accounts.json`) exists in the state
directory, otherwise the file store when `credentials.json` exists there,
otherwise the keyring. Commands that share a state directory therefore use the
same accounts without repeating the flag.
For portable or headless provisioning, one file can hold multiple accounts
across multiple providers. See
[docs/credentials.md](docs/credentials.md) for the complete credential-bundle
workflow and its security requirements.

Manage provider-scoped accounts with:

```sh
# Codex is the default provider.
rute account add -name personal
rute account add -provider=claude -name personal
rute account list
rute account list -provider=claude
rute account history
rute account history -provider=claude personal
rute account reauth -provider=claude personal
rute account remove -provider=claude personal
```

`rute account list` lists every stored account across all supported providers:

```text
PROVIDER  NAME      STATE   UPSTREAM ACCOUNT
codex     personal  active  acct_...
claude    personal  active  -
```

Use `rute account list -provider=codex` or
`rute account list -provider=claude` to filter the output. Account names are
provider-scoped, so the same name can be used once per provider.

Credential changes also append metadata-only events to a private,
hash-chained audit log. `rute account history` verifies the complete chain
before displaying it; add `-output=json` for machine-readable output. The log
contains provider, local record ID, account name, state, action, time, and
chain hashes—never OAuth tokens, authorization codes, PKCE verifiers, or token
fingerprints. See [docs/credentials.md](docs/credentials.md#audit-credential-changes)
for its guarantees and limitations.

Each new session goes to the healthy account with the fewest calls in flight,
and later requests in that session stay on the same account. An account that
needs login or is temporarily rate-limited is excluded until it recovers. Only
authorization and quota failures fail over to another account; upstream `5xx`
responses are not replayed. See
[Balancing several subscriptions](#balancing-several-subscriptions) for
per-account concurrency limits.

When every usable account of a provider is rate-limited, requests for that
provider return HTTP `429` with `error.type: rate_limit_error` and a
`Retry-After` header. The Responses WebSocket sends an `error` event of type
`rate_limit_error` instead. Requests during the cooldown return the same
status without contacting the upstream again.

Claude marks account quota rejections with `Retry-After` or
`anthropic-ratelimit-*` headers. A Claude `429` without either header rejects
that request rather than the account, so rute returns it unchanged and does not
cool down the account or try another one. Claude subscription accounts answer
this way to requests that do not come from Claude Code, for example plain
`curl` calls.

rute logs one stderr line per upstream quota rejection: the account name, the upstream error type and message, and the
rate-limit headers, such as which window (`5h` or `7d`) was exhausted and when
it resets. The retry
interval is the proxy's earliest account cooldown, not a guarantee that the
upstream quota will have reset. If the upstream supplies no reset hint, rute
uses a one-minute cooldown. Authentication or other availability failures
remain distinct from rate limiting. Successful OAuth login does not guarantee
that the account currently has inference capacity.

On a fresh installation, running `rute` starts a Codex login. To start with
Claude instead, add a Claude account first:

```sh
rute account add -provider=claude -name default
rute
```

Each login prints a provider authorization URL and waits for its loopback
callback. Refresh tokens are stored under separate `rute/codex` and
`rute/claude` keyring services.

### Balancing several subscriptions

Subscription accounts slow down sharply as the number of concurrent calls on
one account grows: time to first token rises and output speed falls, while the
account's total throughput stays roughly flat. Spreading the same load across
more accounts keeps each account in its fast range.

Add every subscription as its own account:

```sh
rute account add -provider=claude -name default
rute account add -provider=claude -name personal
rute account add -provider=codex -name codex-a
rute account add -provider=codex -name codex-b
```

`-claude-balance` and `-codex-balance` choose how calls use those accounts.
In every mode, an account that is rate-limited or needs login is skipped until
it recovers, and the call goes to the next usable account without the client
seeing the failure.

| Mode | Where a call goes |
| --- | --- |
| `spread` (default) | New sessions go to the account with the fewest calls in flight. |
| `priority` | The first account in the order with fewer calls in flight than `-*-max-in-flight`. |
| `failover` | The first usable account in the order, however busy it is. |

`-claude-account-order` and `-codex-account-order` list account names from
most to least preferred for `priority` and `failover`. Accounts not listed
follow in the order they were added.

Spread load across equal subscriptions, keeping each one in its fast range:

```sh
rute serve -claude-balance=spread -claude-max-in-flight=6
```

Use a team plan first and a personal plan only for overflow:

```sh
rute serve -claude-balance=priority -claude-account-order=default,personal \
  -claude-max-in-flight=6
```

Use the personal plan only while the team plan is rate-limited:

```sh
rute serve -claude-balance=failover -claude-account-order=default,personal
```

Later requests in a session stay on one account, which keeps that account's
prompt cache warm. Send a stable `X-Session-ID` header per agent run when the
client supports it. A session moves to another account in these cases:

- its account becomes rate-limited or unusable;
- in `spread` and `priority`, its account reaches `-*-max-in-flight` while
  another account is below the limit; or
- in `priority` and `failover`, a more preferred account becomes usable
  again, for example after a quota reset.

Moving a session costs one uncached request on the new account. When every
account is at the limit, calls go to the least loaded account; rute does not
queue or reject them. `priority` requires a `-*-max-in-flight` limit.

The limits above are a starting point from one set of agent benchmarks.
Measure your own with the tools below.

### Management dashboard and API

`rute serve` also listens on `127.0.0.1:20129` for management. A rute
started by `rute claude`, `rute codex`, or `rute admin` picks free ports
instead; `rute admin` opens its dashboard. Open http://127.0.0.1:20129 for a
dashboard that refreshes every five seconds and shows:

- each provider's balance mode and account counts;
- each account's state, cooldown end and reason (for example `seven_day`),
  calls in flight, and call statistics;
- calls grouped by calls in flight, for the last 15 minutes, hour, day, or
  week; and
- the most recent calls.

**Add account** and **log in again** run the same OAuth login as
`rute account add` and `rute account reauth`, and the account joins the
running pool without a restart. Removing accounts stays in the CLI.

**change routing** sets a provider's balance mode, account order, and max in
flight, with the same rules as the `-<provider>-balance`,
`-<provider>-account-order`, and `-<provider>-max-in-flight` flags. The change
applies to new calls at once and is saved to `routing.json` in the state
directory (`-routing-file`). At startup, rute uses the saved routing for a
provider unless any of that provider's routing flags is given.

The same data is available as JSON:

| Request | Returns |
| --- | --- |
| `GET /v1/status` | version, uptime, and each provider's balance mode, order, limit, and accounts |
| `GET /v1/calls/summary?window=1h` | call statistics by account and calls in flight (`15m`, `1h`, `24h`, `7d`) |
| `GET /v1/calls/recent?limit=50` | the newest calls |
| `POST /v1/logins` with `{"provider":"claude","name":"personal","action":"add"}` | a login with its `authorization_url`; `action` is `add` or `reauth` |
| `GET /v1/logins/{id}` | the login's `state`: `pending`, `done`, or `failed` |
| `GET /v1/usage?window=7d&by=day` | tokens and estimated cost per period, account, and model (`window`: `24h`, `7d`, `30d`; `by`: `hour`, `day`, `total`) |
| `POST /v1/restart` with `{}` | restarts rute on the same ports (202); 503 when it was started with `-server-file=off` |
| `PUT /v1/providers/claude/routing` with `{"balance":"priority","order":["default","personal"],"max_in_flight":6}` | the provider's new status; invalid routing returns 400 and changes nothing |

The management port never contains credentials, but it can add accounts, so
it is loopback-only and separate from the proxy port. Do not forward it with
Tailscale Serve. It rejects requests whose `Host` is not a loopback name, the
dashboard's forms carry a per-process token, and the `POST` and `PUT` API
requests require a JSON body. Use `-admin-listen=127.0.0.1:PORT` to move it or
`-admin-listen=off` to disable it.

#### Usage and estimated cost

http://127.0.0.1:20129/usage reports calls and tokens per account and
model, and per hour or day, for the last 24 hours, 7 days, or 30 days. Tokens
are split into input, output, cache read, and cache write, as the provider
reports them. Input excludes cache reads, so each kind can be priced on its
own. `rute stats -usage=day -window=168h` prints the same report.

Neither provider returns a price, and subscriptions are not billed per token.
To see what the same tokens would cost on the API, list per-million-token
rates in `prices.json` in the state directory (or pass `-prices=PATH`). Take
the rates from the provider's pricing page. A name ending in `*` matches a
model family, and the longest match wins:

```json
{
  "models": {
    "example-model-*": { "input": 1.0, "output": 5.0, "cache_read": 0.1, "cache_write": 1.25 }
  }
}
```

rute reloads the file when it changes. Models without a rate show `-` and are
listed under the report.

Call history is stored as per-minute counters in `metrics.db` in rute's state
directory (`~/Library/Application Support/rute/` on macOS,
`~/.local/state/rute/` on Linux). Each row counts calls, outcomes, failovers,
and tokens for one account, model, and calls-in-flight range, with histograms
for wait and tokens per second. Prompts, responses, and individual calls are
never written to disk, so the file stays small. Wait and tok/s percentiles are
estimates from the histograms.

Counters are written every 10 seconds and kept for 30 days
(`-metrics-retention`). `-metrics-db=PATH` moves the file and
`-metrics-db=off` disables it. `rute stats` without file arguments reads the
same database. The dashboard's recent-calls list holds the last 1000 calls in
memory and starts empty after a restart. For per-call records, use
`-metrics-file`.

### Measuring latency under concurrency

`-metrics-file` appends one JSON line per inference call. A line contains the
provider, endpoint, model, account name, calls in flight on that account when
the call started, status, outcome, time to first token (`wait_ms`), duration,
output tokens, and output tokens per second after the first token. It never
contains prompts, outputs, or credentials.

```sh
rute serve -metrics-file="$HOME/Library/Logs/rute/calls.jsonl"
```

`rute stats` groups those lines by calls in flight:

```sh
rute stats -by=provider,account "$HOME/Library/Logs/rute/calls.jsonl"
```

Each row is one group and in-flight range, with these columns:

```text
GROUP  IN FLIGHT  CALLS  OK  429  CANCEL  ERROR  WAIT P50  WAIT P90  TOK/S P50  MEAN CALL
```

`-buckets` sets the in-flight ranges (default `1,3,6,12,24,48`).

`CANCEL` counts calls that the client abandoned, which is how a benchmark
harness's per-task timeout appears. Check it next to the pass rate: a high
cancel rate means the run measured rate limiting rather than the model.

`rute bench` generates the load itself. Each worker makes `-rounds`
sequential calls in its own session, and each `-concurrency` level runs after
the previous one finishes:

```sh
rute bench -api=messages -model=claude-sonnet-5 -concurrency=1,3,6,12 -rounds=3
rute bench -api=responses-ws -model=gpt-5.6-sol -concurrency=1,6,12 -effort=low
```

`-api` is `messages`, `chat`, `responses`, or `responses-ws`. The summary
table adds `TOTAL TOK/S`, the output tokens per second across all workers at
that level. Use `-output=FILE` to keep the per-call lines for `rute stats`.

`rute bench` sends plain API requests. Claude subscription accounts reject
those, so for Claude subscriptions, run your Claude Code workload with
`ANTHROPIC_BASE_URL=http://127.0.0.1:20128` and summarize `-metrics-file`
with `rute stats` instead. `rute bench` works for Codex subscriptions and for
the provider APIs.

To compare with the provider API, point the same command at the API and pass
the key through an environment variable:

```sh
rute bench -url=https://api.anthropic.com -api-key-env=ANTHROPIC_API_KEY \
  -api=messages -model=claude-sonnet-5 -concurrency=1,3,6,12
```

Benchmark load uses the same subscription quota as other work.

### Model routing

Unprefixed model IDs route to Codex. Prefix a model with `claude/` or `codex/`
to select a provider explicitly:

```text
gpt-5.6-sol                 -> Codex model gpt-5.6-sol
codex/gpt-5.6-sol           -> Codex model gpt-5.6-sol
claude/claude-sonnet-5      -> Claude model claude-sonnet-5
```

The native `/v1/messages` endpoint treats an unprefixed model as Claude, while
still honoring explicit model mappings.

Use `-models-file` to expose stable public aliases and map each alias to an
upstream provider/model pair:

```json
[
  {
    "id": "coding",
    "provider": "codex",
    "upstream_model": "gpt-5.6-sol",
    "display_name": "Codex"
  },
  {
    "id": "worker-sonnet",
    "provider": "claude",
    "upstream_model": "claude-sonnet-5",
    "display_name": "Claude Sonnet"
  }
]
```

```sh
rute -models-file=/absolute/path/models.json
```

The file must be a JSON array. `id`, `provider`, and `upstream_model` are
required; `provider` must be `codex` or `claude`. Configured aliases are
returned by `/v1/models` and take precedence over namespace routing.

### HTTP examples

These examples show the request formats. With Claude subscription accounts,
the Claude requests below are rejected because they do not come from Claude
Code; see the notice at the top of this section.

```sh
curl http://127.0.0.1:20128/v1/responses \
  -H 'Content-Type: application/json' \
  --data '{"model":"gpt-5.6-sol","input":"Say hello","stream":false}'
```

```sh
curl http://127.0.0.1:20128/v1/chat/completions \
  -H 'Content-Type: application/json' \
  --data '{
    "model":"claude/claude-sonnet-5",
    "messages":[{"role":"user","content":"Say hello"}],
    "stream":false
  }'
```

The Claude Chat Completions adapter supports text, system/developer messages,
HTTP(S) and base64 images, function tools and results, tool choice, streaming,
and usage. It rejects unsupported OpenAI-only request fields instead of
silently changing their meaning. Claude models are not currently supported by
the Responses HTTP or WebSocket endpoints.

Native Claude Messages requests can be sent without translation:

```sh
curl http://127.0.0.1:20128/v1/messages \
  -H 'Content-Type: application/json' \
  --data '{
    "model":"claude-sonnet-5",
    "max_tokens":1024,
    "messages":[{"role":"user","content":"Say hello"}]
  }'
```

### Responses WebSocket

Connect to `ws://127.0.0.1:20128/v1/responses` and send one
`response.create` JSON object at a time:

```json
{"type":"response.create","model":"gpt-5.6-sol","input":"Say hello"}
```

The proxy makes a Codex HTTP streaming request and forwards each Responses SSE
event as a WebSocket text message. Requests on a connection are processed
sequentially. Because the Codex request is forced to `store: false`,
`previous_response_id` is not supported; resend the input and prior output items
needed for each turn.

### OpenWorker

[OpenWorker](https://github.com/andrewyng/openworker) can use rute's Codex
accounts through its OpenAI-compatible Chat Completions client. Claude
subscription accounts reject its requests.

1. Start rute and complete the first OAuth login.
2. In OpenWorker, choose **OpenAI**.
3. Enter any non-empty placeholder key, such as `rute-local`.
4. Set **Custom endpoint** to `http://127.0.0.1:20128/v1`.
5. Click **Test**.

OpenWorker requires a non-empty key to construct its SDK client, but rute
ignores downstream authorization on its loopback listener and substitutes the
selected account's upstream OAuth access token. Do not expose this listener
outside the local machine.

### Claude Code

Point Claude Code (CLI or app) at rute:

```sh
ANTHROPIC_BASE_URL=http://127.0.0.1:20128 claude
```

Claude Code sends its own requests. rute forwards their body and `anthropic-*`
and `x-claude-code-*` headers unchanged, replaces only the credential with the
selected account's token, and moves to another account when one is
rate-limited. The Claude Agent SDK runs Claude Code, so the same variable works
for it.

### Codex CLI

Codex CLI can use the proxy as a custom Responses provider:

```toml
model_provider = "rute"

[model_providers.rute]
name = "Rute local proxy"
base_url = "http://127.0.0.1:20128/v1"
env_key = "RUTE_DOWNSTREAM_KEY"
wire_api = "responses"
supports_websockets = true
```

Set `RUTE_DOWNSTREAM_KEY` to any non-empty placeholder. Codex CLI requires the
provider credential setting, but `rute` ignores the downstream Authorization
header. Set `supports_websockets = false` to use HTTP SSE instead.

### Supplying credentials non-interactively

The recommended multi-account, multi-provider workflow is to complete OAuth
once per account and write every account into one private credential bundle:

```sh
install -d -m 0700 /absolute/private/path

rute account add \
  -provider=codex \
  -name=codex-primary \
  -credential-store=file \
  -credential-file=/absolute/private/path/credentials.json

rute account add \
  -provider=claude \
  -name=claude-primary \
  -credential-store=file \
  -credential-file=/absolute/private/path/credentials.json

rute account list \
  -credential-store=file \
  -credential-file=/absolute/private/path/credentials.json
```

Provisioning opens the browser once for each account. Subsequent starts are
non-interactive:

```sh
rute \
  -credential-store=file \
  -credential-file=/absolute/private/path/credentials.json
```

The file contains refresh tokens, not a GCP-style immutable service-account
key. It must remain `0600`, must not be committed or logged, and must stay
writable so rute can persist rotated refresh tokens. Use one running rute
process per credential bundle. Rute writes redacted history beside it at
`credentials.json.audit.jsonl`; do not use copies of old secret-bearing
credential bundles as an audit mechanism.

For backward-compatible single-account Codex automation, the proxy also
accepts:

- `RUTE_CODEX_ACCESS_TOKEN`
- `RUTE_CODEX_REFRESH_TOKEN`
- `RUTE_CODEX_ID_TOKEN`
- `RUTE_CODEX_EXPIRES_AT` (optional RFC3339 timestamp)

When `RUTE_CODEX_ACCESS_TOKEN` is set, the environment credential is exclusive
and ephemeral: persistent Codex and Claude accounts are not loaded, and rotated
refresh tokens are not written back anywhere. Environment mode therefore does
not provide multi-account or multi-provider operation.

See [docs/credentials.md](docs/credentials.md) for provisioning, deployment,
rotation, and recovery details.

## PKCE library

The `oauthpkce` package owns a short-lived loopback callback server and the code
exchange while leaving browser launch and token persistence to its caller. A
provider can supply a `TokenExchanger` when its token endpoint does not use the
standard form-encoded OAuth exchange; Claude uses this extension for its JSON
exchange.

```go
package main

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/dio/rute/oauthpkce"
	"github.com/dio/rute/providers/codex"
)

func main() {
	client, err := oauthpkce.New(codex.Config())
	if err != nil {
		log.Fatal(err)
	}

	session, err := client.Start()
	if err != nil {
		log.Fatal(err)
	}
	defer session.Close()

	fmt.Printf("Open this URL in your browser:\n%s\n", session.AuthorizationURL())

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	tokens, err := session.Wait(ctx)
	if err != nil {
		log.Fatal(err)
	}

	// Persist only tokens.RefreshToken in a native credential store. Keep access
	// and ID tokens in memory and never print them.
	_ = tokens
}
```

The library:

- uses a fresh verifier and state for every session;
- supports only the PKCE S256 method;
- binds the callback listener to the loopback interface;
- validates callback state before accepting a code;
- exchanges the code with a context-aware HTTP request.

The library itself does not launch a browser, persist or refresh tokens, proxy
model requests, or issue downstream API keys. Those lifecycle responsibilities
belong to `cmd/proxy` and its internal credential manager.

## Provider notice

The Codex and Claude profiles mirror observed first-party CLI protocols. They
are not official compatibility promises. OAuth client behavior, endpoints,
headers, and request rules can change. Callers are responsible for complying
with each provider's terms and account policies. Rute deliberately sends only
the protocol headers needed for OAuth access; it does not spoof a first-party
CLI's operating system, runtime, SDK, or user-agent fingerprint.

## Development

```sh
make check
```

CI additionally runs golangci-lint, `govulncheck`, and CGO-free cross-builds for
every release target.

To implement another OAuth-backed upstream, follow
[docs/adding-a-provider.md](docs/adding-a-provider.md).

Release preparation, tagging, publication, and artifact verification are
documented in [docs/releasing.md](docs/releasing.md).
