#!/usr/bin/env bash
# Installs the pinned development tools into ./bin from official prebuilt
# release archives, verifying each archive against a pinned SHA-256.
# Idempotent: a tool that already reports the pinned version is left alone.
set -euo pipefail

GOLANGCI_LINT_VERSION="2.15.0"

# Official checksums from
# https://github.com/golangci/golangci-lint/releases/download/v2.15.0/golangci-lint-2.15.0-checksums.txt
declare -A GOLANGCI_LINT_SHA256=(
	[linux-amd64]="9b7fe6a6bf6334b98decf8d86b11415ea7889afdabdd1959fdce23af2a9ebcea"
	[linux-arm64]="b4d8f17068b35ee6060cc2f69a16fb48740d706fbba81661c318649c114a6998"
	[darwin-amd64]="767b899a28285e88ded2823d5dcc4bc5ef681173eb2d80f029a7b17ee4ece3b8"
	[darwin-arm64]="d1e409481a8ca846c4bc97e781eb5cb1bd5c23353a0b3977a1cbda1b1b6447d5"
)

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
bin="${root}/bin"
mkdir -p "${bin}"

os="$(uname -s | tr '[:upper:]' '[:lower:]')"
case "$(uname -m)" in
x86_64 | amd64) arch=amd64 ;;
aarch64 | arm64) arch=arm64 ;;
*)
	echo "bootstrap: unsupported architecture $(uname -m)" >&2
	exit 1
	;;
esac
platform="${os}-${arch}"

sha256() {
	if command -v sha256sum >/dev/null 2>&1; then
		sha256sum "$1" | cut -d' ' -f1
	else
		shasum -a 256 "$1" | cut -d' ' -f1
	fi
}

install_golangci_lint() {
	local target="${bin}/golangci-lint"
	if [[ -x "${target}" ]] && "${target}" --version 2>/dev/null | grep -q "version ${GOLANGCI_LINT_VERSION} "; then
		echo "bootstrap: golangci-lint ${GOLANGCI_LINT_VERSION} already installed"
		return
	fi
	local want="${GOLANGCI_LINT_SHA256[${platform}]:-}"
	if [[ -z "${want}" ]]; then
		echo "bootstrap: no pinned golangci-lint checksum for ${platform}" >&2
		exit 1
	fi
	local name="golangci-lint-${GOLANGCI_LINT_VERSION}-${platform}"
	local url="https://github.com/golangci/golangci-lint/releases/download/v${GOLANGCI_LINT_VERSION}/${name}.tar.gz"
	local tmp
	tmp="$(mktemp -d)"
	trap 'rm -rf "${tmp}"' RETURN
	echo "bootstrap: downloading ${url}"
	curl -sSfL --retry 3 -o "${tmp}/archive.tar.gz" "${url}"
	local got
	got="$(sha256 "${tmp}/archive.tar.gz")"
	if [[ "${got}" != "${want}" ]]; then
		echo "bootstrap: checksum mismatch for ${name}: got ${got}, want ${want}" >&2
		exit 1
	fi
	tar -xzf "${tmp}/archive.tar.gz" -C "${tmp}"
	install -m 0755 "${tmp}/${name}/golangci-lint" "${target}"
	echo "bootstrap: installed $("${target}" --version)"
}

install_golangci_lint
"${bin}/golangci-lint" --version
