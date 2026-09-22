#!/usr/bin/env bash
# Build and package release artifacts the way CLIProxyAPI's plugin store expects.
#
#   scripts/release.sh <version> [goos/goarch ...]
#   scripts/release.sh 0.1.0                        # the host platform
#   scripts/release.sh 0.1.0 linux/arm64 linux/amd64
#
# The version must match internal/buildinfo/buildinfo.go, because the release tag is
# v<version> and CPA compares that version against what the plugin reports.
#
# Output (release/, gitignored):
#   release/clinepass-channel-monitor_<version>_<goos>_<goarch>.zip
#   release/checksums.txt
#
# CPA installs a release by looking for exactly these assets, so every rule below is
# enforced here instead of being discovered as an install failure:
#   * the release tag is v<version> (for example v0.1.0)
#   * one asset per platform named <id>_<version>_<goos>_<goarch>.zip
#   * each zip holds exactly one dynamic library at its root: <id>.so / .dylib / .dll
#   * checksums.txt uses sha256sum format, one bare file name per line
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$REPO_ROOT"

PLUGIN_ID="clinepass-channel-monitor"
STAGE="$REPO_ROOT/.toolchain/release"
OUT="$REPO_ROOT/release"
GO_BIN="$REPO_ROOT/.toolchain/go/bin/go"

die() {
	echo "release: $*" >&2
	exit 1
}

sha256_tool() {
	if command -v sha256sum >/dev/null 2>&1; then
		echo "sha256sum"
	else
		echo "shasum -a 256"
	fi
}

library_extension() {
	case "$1" in
	darwin) echo "dylib" ;;
	windows) echo "dll" ;;
	*) echo "so" ;;
	esac
}

# cross_cc prints the C compiler for a target that differs from the host, or nothing
# when the default toolchain is right. CGO means a cross target needs its own gcc.
cross_cc() {
	local goos="$1" goarch="$2"
	[ "$goos" = "$HOST_GOOS" ] && [ "$goarch" = "$HOST_GOARCH" ] && return 0
	case "$goos/$goarch" in
	linux/amd64) command -v x86_64-linux-gnu-gcc >/dev/null 2>&1 && echo "x86_64-linux-gnu-gcc" ;;
	linux/arm64) command -v aarch64-linux-gnu-gcc >/dev/null 2>&1 && echo "aarch64-linux-gnu-gcc" ;;
	linux/arm) command -v arm-linux-gnueabihf-gcc >/dev/null 2>&1 && echo "arm-linux-gnueabihf-gcc" ;;
	esac
	return 0
}

[ $# -ge 1 ] || die "usage: scripts/release.sh <version> [goos/goarch ...]"
VERSION="${1#v}"
shift

case "$VERSION" in
[0-9]*) ;;
*) die "version '$VERSION' must start with a digit (the tag form is v${VERSION})" ;;
esac
case "$VERSION" in
*[!0-9A-Za-z.+-]*) die "version '$VERSION' may only contain digits, letters, '.', '+' and '-'" ;;
esac

[ -x "$GO_BIN" ] || die "the pinned Go toolchain is missing; run 'make tools' first"
DECLARED="$(sed -n 's/.*Version[[:space:]]*=[[:space:]]*"\([^"]*\)".*/\1/p' internal/buildinfo/buildinfo.go | head -1)"
[ -n "$DECLARED" ] || die "cannot read the declared version from internal/buildinfo/buildinfo.go"
[ "$DECLARED" = "$VERSION" ] ||
	die "internal/buildinfo/buildinfo.go declares $DECLARED, not $VERSION; bump the declared version (or release v$DECLARED)"

HOST_GOOS="$("$GO_BIN" env GOOS)"
HOST_GOARCH="$("$GO_BIN" env GOARCH)"
if [ $# -eq 0 ]; then
	set -- "$HOST_GOOS/$HOST_GOARCH"
fi

mkdir -p "$OUT"
built_any=0
for target in "$@"; do
	goos="${target%%/*}"
	goarch="${target##*/}"
	[ "$goos" != "$target" ] && [ -n "$goarch" ] || die "target '$target' must look like <goos>/<goarch>"
	case "$goos/$goarch" in
	linux/amd64 | linux/arm64 | linux/arm | darwin/amd64 | darwin/arm64 | windows/amd64 | windows/arm64) ;;
	*) die "unsupported platform '$goos/$goarch'" ;;
	esac
	if [ "$goos" = "darwin" ] && [ "$HOST_GOOS" != "darwin" ]; then
		die "darwin artifacts need a macOS host (the CGO linker wants the macOS SDK); build them in CI"
	fi
	cc="$(cross_cc "$goos" "$goarch")"
	if [ "$goos" != "darwin" ] && { [ "$goos" != "$HOST_GOOS" ] || [ "$goarch" != "$HOST_GOARCH" ]; } && [ -z "$cc" ]; then
		die "no C cross compiler for $goos/$goarch; install one (for example gcc-aarch64-linux-gnu) or build it in CI"
	fi

	extension="$(library_extension "$goos")"
	library="$PLUGIN_ID.$extension"
	archive="$OUT/${PLUGIN_ID}_${VERSION}_${goos}_${goarch}.zip"
	echo "release: building $goos/$goarch (version $VERSION, cc=${cc:-default})"
	rm -rf "$STAGE"
	mkdir -p "$STAGE/dist" "$STAGE/pkg"
	CC="${cc:-}" make build BUILD_DIR="$STAGE/dist" GOOS="$goos" GOARCH="$goarch" VERSION="$VERSION" DEV_BUMP=0 >/dev/null
	# The Makefile always names the library .so; the name inside the zip is what matters.
	[ -f "$STAGE/dist/$PLUGIN_ID-v$VERSION.so" ] || die "expected $STAGE/dist/$PLUGIN_ID-v$VERSION.so"
	install -m 0644 "$STAGE/dist/$PLUGIN_ID-v$VERSION.so" "$STAGE/pkg/$library"
	rm -f "$archive"
	(cd "$STAGE/pkg" && zip -q -X "$archive" "$library")

	python3 - "$archive" "$library" <<'PY'
import sys, zipfile

archive_path, wanted = sys.argv[1], sys.argv[2]
with zipfile.ZipFile(archive_path) as archive:
    names = archive.namelist()
if names != [wanted]:
    sys.exit(f"release: {archive_path} must contain only {wanted}, found {names}")
PY
	built_any=$((built_any + 1))
done

[ "$built_any" -gt 0 ] || die "nothing was built"
# Bare file names: CPA looks the archive name up in this file, so a "./" prefix would
# turn into "checksum not found" at install time.
(cd "$OUT" && rm -f checksums.txt && $(sha256_tool) ./*.zip | sed 's| \./| |' | LC_ALL=C sort -k2 >checksums.txt)

echo "release: artifacts in $OUT"
(cd "$OUT" && for zip in ./*.zip; do printf '  %s  %s bytes\n' "${zip#./}" "$(wc -c <"$zip")"; done)
echo "release: checksums.txt"
sed 's/^/  /' "$OUT/checksums.txt"
echo
echo "release: tag and push to publish every platform through .github/workflows/release.yml:"
echo "  git tag v$VERSION && git push origin v$VERSION"
echo "release: to publish these local artifacts instead:"
echo "  gh release create v$VERSION --title v$VERSION --generate-notes release/*.zip release/checksums.txt"
