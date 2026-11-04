#!/bin/sh

set -eu

CDPATH=
export CDPATH

tag=${1:?usage: publish-homebrew-formula.sh TAG}

if ! printf '%s\n' "$tag" | grep -Eq '^v[0-9]+\.[0-9]+\.[0-9]+$'; then
	printf 'Skipping Homebrew formula for prerelease tag %s\n' "$tag"
	exit 0
fi

if [ -z "${HOMEBREW_TAP_PRIVATE_KEY:-}" ]; then
	printf '%s\n' "HOMEBREW_TAP_PRIVATE_KEY is required" >&2
	exit 1
fi

repo_root=$(cd -- "$(dirname "$0")/.." && pwd)
version=${tag#v}
checksums_file="$repo_root/dist/checksums.txt"
template="$repo_root/packaging/homebrew/rute.rb.tmpl"
tap_repository=${HOMEBREW_TAP_REPOSITORY:-ssh://git@github.com/dio/homebrew-tap.git}
tap_clone_repository=${HOMEBREW_TAP_CLONE_REPOSITORY:-https://github.com/dio/homebrew-tap.git}
tap_name=${HOMEBREW_TAP_NAME:-dio/tap}

# checksum_for prints the SHA-256 of the release archive for an OS and
# architecture, such as darwin_arm64.
checksum_for() {
	archive="rute_${version}_$1.tar.gz"
	checksum=$(awk -v name="$archive" '$2 == name { print $1 }' "$checksums_file")
	if ! printf '%s\n' "$checksum" | grep -Eq '^[0-9a-f]{64}$'; then
		printf 'Missing or invalid checksum for %s\n' "$archive" >&2
		exit 1
	fi
	printf '%s\n' "$checksum"
}

sha_darwin_arm64=$(checksum_for darwin_arm64)
sha_darwin_amd64=$(checksum_for darwin_amd64)
sha_linux_arm64=$(checksum_for linux_arm64)
sha_linux_amd64=$(checksum_for linux_amd64)

work_dir=$(mktemp -d "${TMPDIR:-/tmp}/rute-homebrew.XXXXXX")
trap 'rm -rf "$work_dir"' EXIT HUP INT TERM

key_file="$work_dir/deploy-key"
umask 077
printf '%s\n' "$HOMEBREW_TAP_PRIVATE_KEY" >"$key_file"
umask 022

GIT_SSH_COMMAND="ssh -i $key_file -o IdentitiesOnly=yes -o StrictHostKeyChecking=accept-new"
export GIT_SSH_COMMAND
brew tap "$tap_name" "$tap_clone_repository"
# Current Homebrew refuses to load formulae from a tap it has not been told to
# trust. Older versions have no brew trust command and need nothing.
if brew help trust >/dev/null 2>&1; then
	brew trust --tap "$tap_name"
fi
tap_dir=$(brew --repository "$tap_name")
git -C "$tap_dir" remote set-url origin "$tap_repository"
git -C "$tap_dir" pull --ff-only origin main

if ! git -C "$tap_dir" diff --quiet || ! git -C "$tap_dir" diff --cached --quiet; then
	printf 'Homebrew tap checkout is dirty: %s\n' "$tap_dir" >&2
	exit 1
fi

formula="$tap_dir/Formula/rute.rb"
sed \
	-e "s|@TAG@|$tag|g" \
	-e "s|@VERSION@|$version|g" \
	-e "s|@SHA256_DARWIN_ARM64@|$sha_darwin_arm64|g" \
	-e "s|@SHA256_DARWIN_AMD64@|$sha_darwin_amd64|g" \
	-e "s|@SHA256_LINUX_ARM64@|$sha_linux_arm64|g" \
	-e "s|@SHA256_LINUX_AMD64@|$sha_linux_amd64|g" \
	"$template" >"$formula"

ruby -c "$formula"
brew style "$formula"
brew audit --strict "$tap_name/rute"

git -C "$tap_dir" config user.name "rute release bot"
git -C "$tap_dir" config user.email "rute-release@users.noreply.github.com"
git -C "$tap_dir" add Formula/rute.rb

if git -C "$tap_dir" diff --cached --quiet; then
	printf 'Homebrew formula already matches %s\n' "$tag"
	exit 0
fi

git -C "$tap_dir" commit -m "rute formula $tag"
git -C "$tap_dir" push origin main
