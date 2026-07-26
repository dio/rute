# Releasing rute

This document is for maintainers publishing a tagged rute release.

## Prerequisites

- Go matching `go.mod`
- GoReleaser 2.17 or newer
- Syft
- push access to `dio/rute`

The repository's GitHub Actions secrets must provide
`HOMEBREW_TAP_PRIVATE_KEY`, with write access to `dio/homebrew-tap`.

## Local validation

Start from a clean worktree on the commit intended for release:

```sh
make check
make vuln
make release-check
make snapshot
```

Inspect the snapshot archives and checksum file under `dist/`. A snapshot does
not publish a GitHub release or update Homebrew.

## Publish

Create and push an annotated semantic-version tag:

```sh
git tag -a v0.1.0 -m "rute v0.1.0"
git push origin v0.1.0
```

The tag starts `.github/workflows/release.yml`. The workflow:

1. validates the semantic-version tag;
2. reruns `make check`;
3. builds CGO-free macOS and Linux archives for `amd64` and `arm64`;
4. ad-hoc signs and verifies the macOS binaries;
5. embeds version metadata;
6. generates per-archive SBOMs and SHA-256 checksums;
7. publishes the GitHub release;
8. updates the `dio/homebrew-tap` formula; and
9. records GitHub build provenance for the published checksums.

Do not move or recreate a published tag. If a release is defective, fix it on
`main` and publish a new semantic version.

## Verify published artifacts

Download and verify a release with GitHub CLI:

```sh
gh release download v0.1.0 --repo dio/rute --dir rute-release
(cd rute-release && shasum -a 256 -c checksums.txt)
gh attestation verify \
  rute-release/rute_0.1.0_darwin_arm64.tar.gz \
  --repo dio/rute
```

Confirm the matching Homebrew formula was updated:

```sh
brew update
brew info dio/tap/rute
brew upgrade rute
rute version
```
