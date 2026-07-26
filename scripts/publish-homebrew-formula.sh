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
source_name="rute_${version}_source.tar.gz"
checksums_file="$repo_root/dist/checksums.txt"
template="$repo_root/packaging/homebrew/rute.rb.tmpl"
tap_repository=${HOMEBREW_TAP_REPOSITORY:-ssh://git@github.com/dio/homebrew-tap.git}
tap_name=${HOMEBREW_TAP_NAME:-dio/tap}

checksum=$(awk -v name="$source_name" '$2 == name { print $1 }' "$checksums_file")
if ! printf '%s\n' "$checksum" | grep -Eq '^[0-9a-f]{64}$'; then
	printf 'Missing or invalid checksum for %s\n' "$source_name" >&2
	exit 1
fi

commit=$(git -C "$repo_root" rev-parse "${tag}^{commit}")
build_date=$(git -C "$repo_root" show -s --format=%cI "$commit")

work_dir=$(mktemp -d "${TMPDIR:-/tmp}/rute-homebrew.XXXXXX")
trap 'rm -rf "$work_dir"' EXIT HUP INT TERM

key_file="$work_dir/deploy-key"
umask 077
printf '%s\n' "$HOMEBREW_TAP_PRIVATE_KEY" >"$key_file"
umask 022

GIT_SSH_COMMAND="ssh -i $key_file -o IdentitiesOnly=yes -o StrictHostKeyChecking=accept-new"
export GIT_SSH_COMMAND
brew tap "$tap_name" "$tap_repository"
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
	-e "s|@SHA256@|$checksum|g" \
	-e "s|@COMMIT@|$commit|g" \
	-e "s|@BUILD_DATE@|$build_date|g" \
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
