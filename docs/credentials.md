# Credential provisioning and non-interactive use

Rute can complete OAuth once and then run without browser interaction. For
multiple accounts or multiple providers, use a private JSON credential bundle.

This is similar to supplying a service-account JSON file operationally, but the
credential semantics are different: the bundle contains OAuth refresh tokens
and is mutable because providers may rotate those tokens.

## Choose a storage mode

| Mode | Accounts and providers | Persistence | Intended use |
| --- | --- | --- | --- |
| Native keyring | Multiple Codex and Claude accounts | Refresh tokens in macOS Keychain or Linux Secret Service | Normal single-user workstation |
| Credential bundle | Multiple Codex and Claude accounts | Refresh tokens in one private JSON file | Headless hosts, portable provisioning, controlled automation |
| Memory | Accounts added only for the running process | None | Temporary testing |
| `RUTE_CODEX_*` environment | One Codex account only | None | Backward-compatible local automation |

Native keyring is the default. Choose the file backend explicitly; rute never
falls back from keyring to plaintext without being asked.

## Create a multi-provider credential bundle

Choose a location outside source trees and shared directories. Its immediate
parent must be private:

```sh
install -d -m 0700 /absolute/private/path
```

Complete OAuth once for each account. Every command must name the same storage
backend and file:

```sh
rute account add \
  -provider=codex \
  -name=codex-primary \
  -credential-store=file \
  -credential-file=/absolute/private/path/credentials.json

rute account add \
  -provider=codex \
  -name=codex-secondary \
  -credential-store=file \
  -credential-file=/absolute/private/path/credentials.json

rute account add \
  -provider=claude \
  -name=claude-primary \
  -credential-store=file \
  -credential-file=/absolute/private/path/credentials.json

rute account add \
  -provider=claude \
  -name=claude-secondary \
  -credential-store=file \
  -credential-file=/absolute/private/path/credentials.json
```

Each `account add` prints an authorization URL and waits for its loopback
callback. The resulting file is created with mode `0600`. Account names are
provider-scoped, so `-name=primary` may be used once for Codex and once for
Claude.

The CLI owns the credential-file schema. Do not hand-author refresh tokens or
edit generated account IDs, timestamps, or state fields.

## Verify the bundle without exposing secrets

List every account:

```sh
rute account list \
  -credential-store=file \
  -credential-file=/absolute/private/path/credentials.json
```

Example:

```text
PROVIDER  NAME              STATE   UPSTREAM ACCOUNT
codex     codex-primary     active  acct_...
codex     codex-secondary   active  acct_...
claude    claude-primary    active  -
claude    claude-secondary  active  -
```

Filter by provider when needed:

```sh
rute account list \
  -provider=claude \
  -credential-store=file \
  -credential-file=/absolute/private/path/credentials.json
```

`account list` never prints refresh or access tokens.

## Audit credential changes

Rute keeps secret state and history separate:

- `credentials.json` contains only the current file-backed account state,
  including the current refresh tokens;
- `credentials.json.audit.jsonl` contains redacted lifecycle events for that
  bundle; and
- keyring storage uses the same audit format beside its private
  `accounts.json` manifest, while refresh tokens remain in the native keyring.

Every successful account add, credential refresh-token rotation,
reauthentication, removal, and transition to `reauth_required` attempts to
append an event. Inspect all providers:

```sh
rute account history \
  -credential-store=file \
  -credential-file=/absolute/private/path/credentials.json
```

Filter by provider and optional account name:

```sh
rute account history \
  -provider=claude \
  -credential-store=file \
  -credential-file=/absolute/private/path/credentials.json \
  claude-primary
```

Use JSON for archival or tooling:

```sh
rute account history \
  -output=json \
  -credential-store=file \
  -credential-file=/absolute/private/path/credentials.json
```

Each event contains only its format version, sequence, UTC timestamp, action,
provider, opaque local record ID, local account name, account state, previous
hash, and hash. It has no fields for access tokens, refresh tokens, ID tokens,
authorization codes, PKCE verifiers, provider error bodies, upstream account
IDs, or token hashes/fingerprints. The audit file and lock are `0600` in the
same `0700` directory as the credential state.

The SHA-256 chain lets `account history` detect modified event contents,
reordering, insertion, and corruption within the history it receives. Reading
or appending fails if verification fails. It is not tamper-proof against a
writer who can replace the whole log, recompute the chain, or remove events
from its tail. Strong tamper evidence requires periodically anchoring the
latest sequence and hash in an independently controlled append-only system.
The local log must therefore support operational review, not compliance claims
that require an external trust boundary.

Credential state remains authoritative. If an audit append fails after a
credential mutation—because the log is corrupt, unavailable, or has reached
its bounded size—rute keeps the successful credential change and writes a
warning to stderr. This is necessary for refresh-token rotation: restoring an
old token merely to roll back the metadata event could destroy access. Monitor
these warnings and treat one as an audit coverage gap.

Do not retain old copies of `credentials.json` merely to create history: every
copy contains usable secret material. Archive the redacted JSON history
instead, protect it as account metadata, and store any external chain anchor
separately.

## Run non-interactively

After provisioning, start rute with the same bundle:

```sh
rute \
  -credential-store=file \
  -credential-file=/absolute/private/path/credentials.json
```

Rute loads all Codex and Claude accounts, obtains access tokens from their
stored refresh tokens, and refreshes them automatically. No browser is opened
while at least one stored account exists.

Account selection remains provider-local:

- a Codex model uses only Codex accounts;
- a Claude model uses only Claude accounts;
- new sessions rotate across healthy accounts for that provider; and
- authorization or quota failures may fail over to another account for the
  same provider.

An unhealthy Claude account is never replaced by a Codex account, or vice
versa.

## Treat the bundle as mutable secret state

The bundle is not an immutable service-account key. Some providers rotate the
refresh token whenever rute obtains a new access token. Rute atomically rewrites
the bundle to persist the replacement.

Operational requirements:

- keep the containing directory at `0700` and the file at `0600`;
- keep the file writable by the rute process;
- run only one rute process against a given credential bundle;
- do not commit, upload, print, or include the file in diagnostic archives;
- do not place it in a container image or release artifact;
- store it on an encrypted disk or encrypted persistent volume; and
- restrict backups and secret-manager access like any long-lived bearer
  credential.

The adjacent audit log is not credential material, but it exposes account
names and operational timing. Keep it private and back it up only according to
that metadata sensitivity.

Read-only Kubernetes Secrets, ConfigMaps, and immutable secret-file mounts are
not suitable as the live credential bundle because refresh-token rotation
cannot be written back. If an external secret manager is the source of truth,
materialize the bundle into private writable persistent storage and arrange for
updated state to be secured after rotation.

File locking protects atomic reads and writes within the store. It does not
make sharing one rotating refresh token across multiple rute processes safe:
two processes can refresh the same account concurrently, invalidating one
another's stored token.

## Reauthenticate, add, or remove accounts

Use the same file flags for every account mutation:

```sh
rute account reauth \
  -provider=claude \
  -credential-store=file \
  -credential-file=/absolute/private/path/credentials.json \
  claude-primary

rute account remove \
  -provider=codex \
  -credential-store=file \
  -credential-file=/absolute/private/path/credentials.json \
  codex-secondary
```

Stop the serving rute process before mutating its bundle from another process.
Restart it afterward so the in-memory account pools match the stored records.

If an account becomes `reauth_required`, complete `account reauth` for that
provider and account name. If the entire bundle may have been disclosed,
revoke the provider sessions, delete the file, and provision fresh accounts.

## Environment-token compatibility mode

Rute also accepts:

- `RUTE_CODEX_ACCESS_TOKEN`
- `RUTE_CODEX_REFRESH_TOKEN`
- `RUTE_CODEX_ID_TOKEN`
- `RUTE_CODEX_EXPIRES_AT` as an optional RFC3339 timestamp

`RUTE_CODEX_ACCESS_TOKEN` activates this mode. The environment credential is:

- Codex-only;
- a single account;
- exclusive, so persistent Codex and Claude accounts are not loaded;
- process-local; and
- unable to persist refresh-token rotation.

Use it only when those constraints are acceptable. For durable
multi-account/multi-provider operation, use a native keyring or writable
credential bundle.
