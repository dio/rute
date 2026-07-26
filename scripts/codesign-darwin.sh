#!/bin/sh

set -eu

target=$1
binary=$2

case "$target" in
darwin_*) ;;
*) exit 0 ;;
esac

if [ "$(uname -s)" != "Darwin" ]; then
	printf '%s\n' "Darwin release binaries must be built and signed on macOS." >&2
	exit 1
fi

/usr/bin/codesign --force --sign - "$binary"
/usr/bin/codesign --verify --strict "$binary"
