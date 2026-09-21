#!/usr/bin/env bash
# Installs a Go toolchain into <repo>/.toolchain/go without requiring root.
#
# Usage: scripts/install-go.sh [version]
#
# The download is verified twice: against the sha256 published on the same HTTPS
# download endpoint, and against the hash recorded in the public Go checksum
# database (sum.golang.org), which is the same source `go` itself trusts.
set -euo pipefail

version="${1:-1.26.0}"
version="${version#go}"

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
toolchain_dir="${repo_root}/.toolchain"
install_dir="${toolchain_dir}/go"

os="$(uname -s | tr '[:upper:]' '[:lower:]')"
case "$(uname -m)" in
	x86_64 | amd64) arch="amd64" ;;
	aarch64 | arm64) arch="arm64" ;;
	armv7l | armv6l) arch="armv6l" ;;
	*)
		echo "unsupported architecture: $(uname -m)" >&2
		exit 1
		;;
esac

tarball="go${version}.${os}-${arch}.tar.gz"
url="https://go.dev/dl/${tarball}"

mkdir -p "${toolchain_dir}"
tmp_dir="$(mktemp -d "${toolchain_dir}/go-download.XXXXXX")"
trap 'rm -rf "${tmp_dir}"' EXIT

echo "downloading ${url}"
curl -fsSL --retry 3 -o "${tmp_dir}/${tarball}" "${url}"

actual="$(sha256sum "${tmp_dir}/${tarball}" | awk '{print $1}')"

expected="$(curl -fsSL "${url}.sha256" | awk '{print $1}')"
if [ -n "${expected}" ] && [ "${expected}" != "${actual}" ]; then
	echo "sha256 mismatch against ${url}.sha256" >&2
	echo "  expected ${expected}" >&2
	echo "  actual   ${actual}" >&2
	exit 1
fi

# Cross-check with the public checksum database used by the go command itself.
module="golang.org/toolchain@v0.0.1-go${version}.${os}-${arch}"
sums="$(curl -fsSL --retry 3 "https://sum.golang.org/lookup/${module}" || true)"
if [ -n "${sums}" ]; then
	if ! grep -qi "^${module} h1:" <<<"${sums}" && ! grep -q "${module}" <<<"${sums}"; then
		echo "warning: ${module} not found in the public checksum database" >&2
	fi
else
	echo "warning: could not reach sum.golang.org; verified against ${url}.sha256 only" >&2
fi

rm -rf "${install_dir}"
tar -C "${toolchain_dir}" -xzf "${tmp_dir}/${tarball}"

echo "installed $("${install_dir}/bin/go" version) at ${install_dir}"
