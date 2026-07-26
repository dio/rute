# Adding an OAuth provider

This guide is for contributors adding a new OAuth-backed model provider to
rute. It is not needed to add another account for an already supported
provider; use:

```sh
rute account add -provider=claude -name personal
rute account list
```

Rute currently has explicit Codex and Claude integrations rather than a
runtime plugin system. Adding a provider is therefore a code change across the
OAuth, credential, routing, proxy, CLI, and test layers.

## Design the integration first

Before writing code, record:

- a stable lowercase provider ID, such as `acme`;
- the authorization, token, refresh, and inference endpoints;
- whether authorization-code exchange and refresh use standard form encoding
  or provider-specific JSON;
- required scopes, authorization parameters, API headers, and model endpoint;
- supported downstream protocols and translation boundaries;
- access-token lifetime, refresh lead time, and refresh-token rotation rules;
- which upstream errors mean reauthentication, rate limiting, or transient
  failure; and
- whether the OAuth client and inference protocol are public contracts or
  observed first-party behavior.

Do not copy operating-system, SDK, runtime, or user-agent fingerprint headers
from another client unless the provider explicitly requires and documents
them. Never log authorization codes, PKCE verifiers, access tokens, refresh
tokens, ID tokens, or provider error bodies that may contain those values.

## 1. Register the provider identity

Add the provider constant in `internal/accountstore/store.go`, then include it
in the provider validation performed by:

- `accountstore.normalizeAccount`;
- `codexpool.NewProvider`;
- `modelcatalog.normalizeModel`; and
- the CLI's provider validation in `cmd/proxy/main.go`.

Provider IDs are persisted in credential-store version 2 and form part of the
account-name namespace. Native secrets use the keyring service
`rute/<provider>`. Do not reuse or rename an existing provider ID: that would
orphan stored credentials.

If a credential-file schema change is necessary, increment its version and
add an explicit backward-compatible migration test. A record with no provider
currently migrates to Codex.

## 2. Add the OAuth profile

Create `providers/<provider>/<provider>.go` with an `oauthpkce.Config`.
Standard authorization-code + PKCE providers usually need only:

```go
func Config() oauthpkce.Config {
	return oauthpkce.Config{
		ClientID:         clientID,
		AuthorizationURL: authorizationURL,
		TokenURL:         tokenURL,
		Scopes:           []string{"inference"},
		Loopback: oauthpkce.LoopbackConfig{
			RedirectHost: "localhost",
			CallbackPath: "/callback",
		},
	}
}
```

The PKCE library always generates fresh state and verifier values, uses S256,
binds its callback server to IPv4 loopback, validates the callback host and
state, and exchanges the code with the session context.

When a provider's token endpoint is not compatible with the standard OAuth
form exchange, implement `oauthpkce.TokenExchanger` and set
`Config.TokenExchanger`. The exchanger receives the validated token URL,
authorization code, state, redirect URI, and verifier. It must:

- use the supplied context and HTTP client;
- set the provider's required content type;
- bound the response body before decoding it;
- reject incomplete token responses;
- return `oauthpkce.TokenExchangeError` for provider rejections; and
- keep response bodies and credential values out of formatted errors.

See `providers/claude` for a JSON exchange with embedded-state validation.

## 3. Implement refresh behavior

If the provider accepts the default JSON refresh request used by
`internal/codexauth`, construct a `codexauth.Config` with its client ID and
token URL. Otherwise create `internal/<provider>auth` and supply a custom
`codexauth.RefreshFunc`.

Refresh implementations must preserve the previous refresh token when the
response omits a replacement, persist rotated refresh tokens, calculate the
new expiry, and map permanent failures such as `invalid_grant` to
`codexauth.ErrReauthenticationRequired`.

Set `RefreshBefore` from observed or documented token lifetime behavior.
The account pool applies stable per-account jitter to avoid refreshing every
account simultaneously.

If the provider has a stable, non-secret account identifier, provide
`Config.Identity`. Otherwise return an empty identity; account names still
remain unique within that provider.

## 4. Wire account management

Update these switches in `cmd/proxy/main.go`:

- `newProviderPool`, which selects the refresh configuration;
- `login`, which selects the PKCE profile;
- `providerDisplayName`; and
- `parseAccountOptions`, including the accepted provider values.

Also include the provider in the unfiltered `account list` provider set.
Keep these behaviors:

- add, reauthenticate, and remove default to Codex for backward compatibility;
- list without `-provider` returns all providers;
- list with `-provider=<id>` filters to one provider; and
- identical account names are allowed across providers, but not twice within
  one provider.

## 5. Register credentials with the proxy

Load a provider-specific `codexpool.Pool` from the shared account store during
startup, bootstrap it independently, run its refresh manager, and add it to the
proxy's credential-source map.

The current `proxy.NewMulti` constructor has explicit Codex and Claude
arguments. For a third provider, prefer changing it to accept a provider-keyed
credential map rather than adding another positional argument. Preserve
`proxy.New` as the backward-compatible Codex-only constructor.

Each request must acquire credentials only from the resolved provider pool.
Session stickiness, authorization refresh/retry, rate-limit cooldown, and
failover should remain provider-local. Do not retry `5xx` responses across
accounts because the upstream may already have processed the request.

## 6. Add protocol routing

Decide which existing downstream endpoints the provider can support:

- native provider endpoint, with minimal transformation;
- OpenAI Chat Completions translation;
- OpenAI Responses HTTP; and
- OpenAI Responses WebSocket.

Do not claim an endpoint until its request, streaming events, errors, tools,
and usage semantics can be translated without silently changing meaning.
Return a clear `unsupported_endpoint` error for unsupported combinations.

For native forwarding:

- remove downstream authorization and API-key headers;
- inject the selected OAuth bearer token;
- forward only explicitly allowed protocol headers;
- set required API version or OAuth capability headers;
- preserve unknown JSON body fields where practical;
- bound request and error bodies; and
- stream without buffering the complete response.

For translated protocols, define and test the supported subset explicitly.
Reject incompatible request fields rather than dropping them.

## 7. Extend model routing

`internal/modelcatalog` resolves models in this order:

1. an exact ID from `-models-file`;
2. a `<provider>/<upstream-model>` namespace; and
3. an unprefixed Codex model.

Add the provider to namespace resolution and model-file validation. An explicit
mapping has this shape:

```json
{
  "id": "worker-fast",
  "provider": "acme",
  "upstream_model": "acme-fast-1",
  "display_name": "Acme Fast"
}
```

The public ID must remain in downstream responses while only
`upstream_model` is sent to the provider. Include configured mappings in
`GET /v1/models`.

## 8. Add tests

Tests must use local `httptest` servers and fake credential stores; CI must not
require live accounts. At minimum, cover:

- authorization URL, scopes, redirect URI, and S256 challenge;
- standard or custom code exchange, including state validation;
- redaction of secret-bearing values and provider errors;
- refresh request encoding, expiry, rotation, and permanent failure;
- provider-scoped file and keyring storage;
- v1 credential migration if storage changes;
- add/list/filter/reauth/remove CLI parsing;
- explicit mapping, namespace routing, and unprefixed fallback;
- downstream auth stripping and upstream OAuth injection;
- native and translated non-streaming responses;
- streaming text, tools, terminal events, errors, and usage;
- authorization retry, rate-limit failover, and no replay on `5xx`; and
- rejection of unsupported endpoint/field combinations.

Run the same checks as CI:

```sh
gofmt -w ./cmd ./internal ./oauthpkce ./providers
go mod tidy
make check
make vuln
golangci-lint run --new-from-rev=HEAD
```

Release binaries must continue to cross-build with `CGO_ENABLED=0` for macOS
and Linux on `amd64` and `arm64`.

## 9. Document compatibility and manual testing

Update the README with:

- account commands and first-login behavior;
- supported endpoint/model combinations;
- namespace and model-file examples;
- known translation limits; and
- a provider compatibility and terms notice.

Then manually test, without printing stored tokens:

1. add one provider account through the browser flow;
2. verify `rute account list` and its provider filter;
3. start the proxy;
4. call each supported endpoint with `curl`;
5. exercise one streaming request and one tool call; and
6. reauthenticate and remove the test account.

Live tests are a release check, not a substitute for deterministic regression
tests.
