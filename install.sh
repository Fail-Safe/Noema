#!/bin/sh
# Install a published binary. The separate zsh ./install builds from source.
# Keep all execution inside main, invoked only after the complete script arrives.
main() {
    set -eu

    fail() { printf 'error: %s\n' "$*" >&2; exit 1; }
    warn() { printf 'warning: %s\n' "$*" >&2; }
    case "${1:-}" in
        --help|-h)
            cat <<'HELP'
Usage: sh install.sh [--help]
Install the latest stable Noema binary for Linux or macOS (amd64/arm64).

Environment:
  NOEMA_VERSION       Specific version, e.g. 0.24.0 or v0.24.0-beta.1
  NOEMA_INSTALL_DIR   Destination directory (default: $HOME/.local/bin)
  NOEMA_SKIP_VERIFY   Set to 1 to allow installation without a SHA-256 tool

Never uses sudo or modifies shell startup files.
HELP
            return 0 ;;
        '') ;;
        *) fail "unknown argument: $1 (use --help)" ;;
    esac
    [ "$#" -le 1 ] || fail 'too many arguments'

    case "$(uname -s)" in
        Linux) os=linux ;;
        Darwin) os=darwin ;;
        *) fail "unsupported OS: $(uname -s)" ;;
    esac
    case "$(uname -m)" in
        x86_64|amd64) arch=amd64 ;;
        aarch64|arm64) arch=arm64 ;;
        *) fail "unsupported architecture: $(uname -m)" ;;
    esac
    if command -v curl >/dev/null 2>&1; then
        downloader=curl
    elif command -v wget >/dev/null 2>&1; then
        downloader=wget
    else
        fail 'curl or wget is required'
    fi
    if command -v sha256sum >/dev/null 2>&1; then
        verifier=sha256sum
    elif command -v shasum >/dev/null 2>&1; then
        verifier=shasum
    elif [ "${NOEMA_SKIP_VERIFY:-0}" = 1 ]; then
        verifier=none
        warn 'no SHA-256 tool found; verification explicitly skipped'
    else
        fail 'sha256sum or shasum is required (NOEMA_SKIP_VERIFY=1 allows skipping)'
    fi

    tmp_dir=$(mktemp -d)
    staged=''
    trap 'rm -rf "$tmp_dir"; if [ -n "$staged" ]; then rm -f "$staged"; fi' 0
    trap 'exit 1' HUP INT TERM
    releases=https://github.com/Fail-Safe/Noema/releases

    # Return 44 only for HTTP 404; all other download failures are fatal.
    download() {
        if [ "$downloader" = curl ]; then
            status=$(curl -sSL -w '%{http_code}' -o "$2" "$1") || return 1
            case "$status" in
                200) return 0 ;;
                404) return 44 ;;
                *) warn "HTTP $status downloading $1"; return 1 ;;
            esac
        else
            if wget -S -O "$2" "$1" 2>"$tmp_dir/wget.log"; then
                return 0
            fi
            status=$(awk '/^[[:space:]]*HTTP\// { code=$2 } END { print code }' "$tmp_dir/wget.log")
            [ "$status" != 404 ] || return 44
            warn "download failed: $1"
            return 1
        fi
    }

    version=${NOEMA_VERSION:-}
    if [ -z "$version" ]; then
        if [ "$downloader" = curl ]; then
            latest=$(curl -fsSL -o /dev/null -w '%{url_effective}' "$releases/latest") || fail 'cannot resolve latest stable release'
        else
            wget -S --spider "$releases/latest" 2>"$tmp_dir/latest.log" || fail 'cannot resolve latest stable release'
            latest=$(sed -n 's/^[[:space:]]*Location: \([^[:space:]]*\).*/\1/p' "$tmp_dir/latest.log" | tail -n 1)
        fi
        version=${latest##*/}
        case "$latest" in
            */releases/tag/v*) ;;
            *) fail 'latest stable release did not resolve to a release tag' ;;
        esac
        case "$version" in *-*) fail 'latest release unexpectedly resolved to a prerelease' ;; esac
    fi
    version=${version#v}
    case "$version" in
        ''|*[!0-9A-Za-z.+-]*|[!0-9]*) fail 'invalid NOEMA_VERSION' ;;
    esac
    base="$releases/download/v$version"
    archive="noema_${version}_${os}_${arch}.tar.gz"
    if [ "$os" = linux ]; then
        musl_archive="noema_${version}_${os}_${arch}_musl.tar.gz"
        if download "$base/$musl_archive" "$tmp_dir/archive.tar.gz"; then
            archive=$musl_archive
        else
            result=$?
            [ "$result" = 44 ] || fail "could not download $musl_archive"
            warn 'static musl archive unavailable for this version; falling back to glibc'
            libc=$(getconf GNU_LIBC_VERSION 2>/dev/null || true)
            if [ -z "$libc" ]; then
                libc=$(ldd --version 2>&1 || true)
            fi
            libc_version=$(printf '%s\n' "$libc" | sed -n '1s/.* \([0-9][0-9]*\.[0-9][0-9]*\).*$/\1/p')
            if printf '%s\n' "$libc" | grep -qi musl; then
                warn 'this system uses musl; the older glibc binary may not run'
            elif [ -z "$libc_version" ] || awk -v v="$libc_version" 'BEGIN { split(v,a,"."); exit !(a[1]<2 || (a[1]==2 && a[2]<39)) }'; then
                warn 'the fallback binary may require glibc 2.39; local glibc is older or could not be determined'
            fi
            download "$base/$archive" "$tmp_dir/archive.tar.gz" || fail "could not download $archive"
        fi
    else
        download "$base/$archive" "$tmp_dir/archive.tar.gz" || fail "could not download $archive"
    fi

    if [ "$verifier" != none ]; then
        download "$base/checksums.txt" "$tmp_dir/checksums.txt" || fail 'could not download checksums.txt'
        expected=$(awk -v name="$archive" '$2 == name || $2 == "*" name { print $1; count++ } END { if (count != 1) exit 1 }' "$tmp_dir/checksums.txt") || fail "missing or duplicate checksum for $archive"
        [ "${#expected}" -eq 64 ] || fail 'invalid SHA-256 checksum'
        case "$expected" in *[!0-9a-fA-F]*) fail 'invalid SHA-256 checksum' ;; esac
        if [ "$verifier" = sha256sum ]; then
            actual=$(sha256sum "$tmp_dir/archive.tar.gz")
        else
            actual=$(shasum -a 256 "$tmp_dir/archive.tar.gz")
        fi
        actual=${actual%% *}
        expected=$(printf '%s' "$expected" | tr 'A-F' 'a-f')
        [ "$actual" = "$expected" ] || fail "SHA-256 mismatch for $archive"
    fi

    tar -xzf "$tmp_dir/archive.tar.gz" -C "$tmp_dir" noema
    [ -f "$tmp_dir/noema" ] && [ ! -L "$tmp_dir/noema" ] || fail 'archive does not contain a regular noema binary'
    install_dir=${NOEMA_INSTALL_DIR:-"$HOME/.local/bin"}
    mkdir -p "$install_dir"
    install_dir=$(cd "$install_dir" && pwd -P)
    staged=$(mktemp "$install_dir/.noema.XXXXXXXX")
    cp "$tmp_dir/noema" "$staged"
    chmod 755 "$staged"
    if [ "$os" = darwin ] && command -v codesign >/dev/null 2>&1; then
        codesign --force --sign - "$staged"
    fi
    # Check compatibility before replacing an existing installation.
    "$staged" version
    mv -f "$staged" "$install_dir/noema"
    staged=''
    printf 'Installed: %s/noema\n' "$install_dir"
    "$install_dir/noema" version
    case ":${PATH:-}:" in
        *:"$install_dir":*) ;;
        *)
            # Single-quote paths safely, including spaces and apostrophes.
            quoted_dir=$(printf '%s' "$install_dir" | sed "s/'/'\\\\''/g")
            # shellcheck disable=SC2016 # Print a command containing literal $PATH.
            printf '\nAdd to PATH (sh/bash/zsh):\n  export PATH='"'%s'"':"$PATH"\n' "$quoted_dir"
            printf 'Add to PATH (fish):\n  fish_add_path '"'%s'"'\n' "$quoted_dir"
            ;;
    esac
    printf '\nNext steps:\n  noema init --name my-cortex\n  noema serve --transport stdio\nAgent setup: https://noemacortex.com/#agent-setup\n'
}
main "$@"
