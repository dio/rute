# rute

`rute` contains a Go OAuth 2.0 Authorization Code + PKCE library and a small
loopback-only OpenAI-compatible proxy backed by a Codex OAuth session.

## Local proxy

Start the proxy:

```sh
go run ./cmd/proxy
```

If no credentials are supplied, it prints an authorization URL and waits up to
five minutes for the loopback OAuth callback. The resulting refresh token is
stored in the native macOS Keychain or Linux Secret Service. Access and ID
tokens stay in memory and are refreshed automatically.

The command listens on `127.0.0.1:20128` and exposes:

- `POST /v1/responses`, streaming or non-streaming
- `GET /v1/responses` with a WebSocket upgrade
- `POST /v1/chat/completions`, streaming or non-streaming
- `GET /v1/models`
- `GET /healthz`

No downstream API key is required or checked. For that reason, `-listen`
rejects wildcard and non-loopback addresses. This command is intended as a
single-user local compatibility layer, not a multi-user gateway or virtual-key
service.

### Credential storage and accounts

Native key storage is the default and never silently falls back to plaintext:

```sh
go run ./cmd/proxy -credential-store=keyring
```

Linux desktop sessions need a working, unlocked Secret Service collection.
For headless systems, explicitly opt into a private JSON file or ephemeral
memory:

```sh
go run ./cmd/proxy -credential-store=file
go run ./cmd/proxy -credential-store=memory
```

The file backend uses `0600` files inside a `0700` state directory. Override
its location with `-credential-file=/absolute/path/credentials.json`.

Manage more than one Codex account with:

```sh
go run ./cmd/proxy account add -name personal
go run ./cmd/proxy account list
go run ./cmd/proxy account reauth personal
go run ./cmd/proxy account remove personal
```

New sessions rotate across healthy accounts and remain pinned to their chosen
account. An account that needs login or is temporarily rate-limited is excluded
until it recovers. Only authorization and quota failures fail over to another
account; upstream `5xx` responses are not replayed.

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
    "model":"gpt-5.6-sol",
    "messages":[{"role":"user","content":"Say hello"}],
    "stream":false
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

The proxy accepts credentials through:

- `RUTE_CODEX_ACCESS_TOKEN`
- `RUTE_CODEX_REFRESH_TOKEN`
- `RUTE_CODEX_ID_TOKEN`
- `RUTE_CODEX_EXPIRES_AT` (optional RFC3339 timestamp)

Treat all token values as secrets. Environment variables are useful for local
automation, but an OS credential store is preferable for long-lived tooling.
When `RUTE_CODEX_ACCESS_TOKEN` is set, the environment credential is exclusive
and ephemeral; rute does not copy it into persistent storage.

## PKCE library

The `oauthpkce` package owns a short-lived loopback callback server and the code
exchange while leaving browser launch and token persistence to its caller.

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

## Codex provider notice

The Codex profile and proxy behavior mirror an observed Codex CLI protocol.
They are not an official compatibility promise. Endpoints and request rules can
change, and callers are responsible for complying with the provider's terms
and account policies.

## Development

```sh
make check
```
