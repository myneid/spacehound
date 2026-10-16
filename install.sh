#!/bin/sh
# SpaceHound installer for GitHub releases.
#
#   sh install.sh --version v0.1.1 [--prefix DIR] [--repo OWNER/NAME]
#
# Downloads the darwin archive for this Mac and the release's SHA256SUMS,
# verifies the checksum, and only then installs the binary. Nothing that is
# downloaded is ever executed or piped into a shell. Read this script before
# running it.
#
# --version latest is resolved from GitHub only when --repo (or
# SPACEHOUND_REPO) names a real hosted repository.
set -eu

REPO="${SPACEHOUND_REPO:-myneid/spacehound}"
VERSION="${SPACEHOUND_VERSION:-}"
PREFIX="${SPACEHOUND_PREFIX:-$HOME/.local/bin}"
NAME=spacehound

die() { printf 'install.sh: %s\n' "$*" >&2; exit 1; }

usage() {
	cat <<EOF
Usage: sh install.sh --version vX.Y.Z [--prefix DIR] [--repo OWNER/NAME]

  --version TAG   release tag to install (required; "latest" needs a hosted --repo)
  --prefix DIR    install directory (default: \$HOME/.local/bin; /usr/local/bin also common)
  --repo O/N      GitHub repository (default: $REPO)
EOF
}

while [ $# -gt 0 ]; do
	case "$1" in
	--version) [ $# -ge 2 ] || die "--version needs a value"; VERSION=$2; shift 2 ;;
	--prefix) [ $# -ge 2 ] || die "--prefix needs a value"; PREFIX=$2; shift 2 ;;
	--repo) [ $# -ge 2 ] || die "--repo needs a value"; REPO=$2; shift 2 ;;
	-h | --help) usage; exit 0 ;;
	*) usage >&2; die "unknown argument: $1" ;;
	esac
done

[ "$(uname -s)" = Darwin ] || die "SpaceHound supports macOS only"
case "$(uname -m)" in
x86_64) ARCH=amd64 ;;
arm64) ARCH=arm64 ;;
*) die "unsupported CPU: $(uname -m)" ;;
esac

case "$REPO" in
OWNER/* | "") REPO_HOSTED=no ;;
*/*) REPO_HOSTED=yes ;;
*) die "--repo must look like OWNER/NAME" ;;
esac

[ -n "$VERSION" ] || { usage >&2; die "an explicit --version is required"; }
if [ "$VERSION" = latest ]; then
	[ "$REPO_HOSTED" = yes ] || die "--version latest needs --repo OWNER/NAME of a published repository"
	# Follow the releases/latest redirect to learn the tag; nothing is executed.
	url=$(curl -fsSLI -o /dev/null -w '%{url_effective}' "https://github.com/$REPO/releases/latest") ||
		die "could not resolve the latest release of $REPO"
	VERSION=${url##*/}
	case "$VERSION" in v[0-9]*) ;; *) die "could not determine latest version (got '$VERSION')" ;; esac
fi
case "$VERSION" in
*/* | *' '* | '') die "invalid version: $VERSION" ;;
esac
[ "$REPO_HOSTED" = yes ] || die "set --repo OWNER/NAME (or SPACEHOUND_REPO) to the repository hosting the release"

command -v curl >/dev/null 2>&1 || die "curl is required"
command -v shasum >/dev/null 2>&1 || die "shasum is required"

ARCHIVE="${NAME}_${VERSION}_darwin_${ARCH}.tar.gz"
BASE="https://github.com/$REPO/releases/download/$VERSION"

TMP=$(mktemp -d "${TMPDIR:-/tmp}/spacehound-install.XXXXXX")
trap 'rm -rf "$TMP"' EXIT INT TERM

printf 'Downloading %s\n' "$BASE/$ARCHIVE"
curl -fsSL --proto '=https' --tlsv1.2 -o "$TMP/$ARCHIVE" "$BASE/$ARCHIVE" || die "download failed"
curl -fsSL --proto '=https' --tlsv1.2 -o "$TMP/SHA256SUMS" "$BASE/SHA256SUMS" || die "checksum download failed"

expected=$(awk -v f="$ARCHIVE" '$2 == f || $2 == "*" f { print $1 }' "$TMP/SHA256SUMS")
[ -n "$expected" ] || die "$ARCHIVE is not listed in SHA256SUMS"
actual=$(shasum -a 256 "$TMP/$ARCHIVE" | awk '{ print $1 }')
[ "$expected" = "$actual" ] || die "checksum mismatch for $ARCHIVE (expected $expected, got $actual); not installing"
printf 'Checksum verified: %s\n' "$actual"

mkdir "$TMP/x"
tar -xzf "$TMP/$ARCHIVE" -C "$TMP/x"
BIN=$(find "$TMP/x" -type f -name "$NAME" -perm -u+x | head -n 1)
[ -n "$BIN" ] || die "archive does not contain the $NAME binary"

mkdir -p "$PREFIX" 2>/dev/null || true
if [ -d "$PREFIX" ] && [ -w "$PREFIX" ]; then
	install -m 0755 "$BIN" "$PREFIX/$NAME"
else
	printf '%s is not writable; using sudo only to copy the verified binary.\n' "$PREFIX"
	sudo install -d -m 0755 "$PREFIX"
	sudo install -m 0755 "$BIN" "$PREFIX/$NAME"
fi

printf 'Installed %s %s to %s\n' "$NAME" "$VERSION" "$PREFIX/$NAME"
case ":$PATH:" in
*":$PREFIX:"*) ;;
*) printf 'Note: %s is not on your PATH. Add it, e.g.: export PATH="%s:$PATH"\n' "$PREFIX" "$PREFIX" ;;
esac
printf 'Try it safely first: %s --demo\n' "$NAME"
