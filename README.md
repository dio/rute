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

## Local proxy

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
devices in your tailnet. With Tailscale installed and signed in on the host:

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
Tailscale Funnel.

On macOS, run `rute serve -listen=127.0.0.1:20128` from a LaunchAgent to keep
the proxy running in the background and start it at login. Tailscale Serve
forwards to that local listener; it does not start `rute` itself. Check the
LaunchAgent logs and `tailscale serve status` when troubleshooting.

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

New sessions rotate across healthy accounts and remain pinned to their chosen
account. An account that needs login or is temporarily rate-limited is excluded
until it recovers. Only authorization and quota failures fail over to another
account; upstream `5xx` responses are not replayed.

On a fresh installation, running `rute` starts a Codex login. To start with
Claude instead, add a Claude account first:

```sh
rute account add -provider=claude -name default
rute
```

Each login prints a provider authorization URL and waits for its loopback
callback. Refresh tokens are stored under separate `rute/codex` and
`rute/claude` keyring services.

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

[OpenWorker](https://github.com/andrewyng/openworker) can use rute through its
OpenAI-compatible Chat Completions client:

1. Start rute and complete the first OAuth login.
2. In OpenWorker, choose **OpenAI**.
3. Enter any non-empty placeholder key, such as `rute-local`.
4. Set **Custom endpoint** to `http://127.0.0.1:20128/v1`.
5. Click **Test**.

OpenWorker requires a non-empty key to construct its SDK client, but rute
ignores downstream authorization on its loopback listener and substitutes the
selected account's upstream OAuth access token. Do not expose this listener
outside the local machine.

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
