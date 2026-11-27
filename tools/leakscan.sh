#!/bin/bash
# Refuses (exit 1) when anything that identifies the owner's car, tailnet, phone or machine is
# found. This repository is PUBLIC: run it before every push and before every publish.
#
#   tools/leakscan.sh --git      every file in the working tree that git would see (tracked or
#                                not ignored) and every object in .git: all branches, all history
#   tools/leakscan.sh PATH...    files and directories; tar archives (CoilShift packages) and
#                                squashfs images (payloads) are scanned unpacked
#
# It prints where a hit is (file or git object, and the count), never the matched text; with
# LEAK_SHOW=1 it prints the matches too (on your own terminal only).
#
# Patterns: the generic ones below, plus LEAK_PATTERN (an extended regex) from local.env, which
# holds the identifiers that cannot be written in a public file: the VIN and its fragments, the
# tailnet name, the phone key and its id/fingerprint, MAC addresses. Without LEAK_PATTERN the
# scan refuses to run, unless LEAK_GENERIC_ONLY=1 (contributors who have no car identifiers).
# EXTRA_LEAK adds patterns for one run (build-payloads.sh adds build paths).
set -euo pipefail

root=$(cd "$(dirname "$0")/.." && pwd)
[ -f "$root/local.env" ] && . "$root/local.env"
if [ -z "${LEAK_PATTERN:-}" ] && [ "${LEAK_GENERIC_ONLY:-}" != 1 ]; then
    echo "leakscan: no LEAK_PATTERN (local.env, see local.env.example); LEAK_GENERIC_ONLY=1 to scan generic patterns only" >&2
    exit 2
fi

# Generic: tailnet host names and addresses (100.64.0.0/10), home-directory paths, private keys,
# access tokens.
generic='[a-z0-9-]\.ts\.net|100\.(6[4-9]|[7-9][0-9]|1[01][0-9]|12[0-7])\.[0-9]{1,3}\.[0-9]{1,3}|/Users/[a-z]|BEGIN [A-Z ]*PRIVATE KEY|tskey-[a-z]|gh[pousr]_[A-Za-z0-9]{20}|AKIA[0-9A-Z]{16}'
pattern=$generic${LEAK_PATTERN:+|$LEAK_PATTERN}${EXTRA_LEAK:+|$EXTRA_LEAK}
# Matches that are not personal: the tailnet range itself and the gateway tests' made-up addresses.
allow='100\.64\.0\.0|100\.100\.1\.1|100\.101\.1\.1'

tmp=$(mktemp -d "${TMPDIR:-/tmp}/leakscan.XXXXXX")
trap 'rm -rf "$tmp"' EXIT
hits=0

report() { # LABEL: reads text on stdin
    local m n
    m=$(LC_ALL=C grep -aoiE -- "$pattern" | LC_ALL=C grep -aviE -x -- "$allow" || true)
    [ -n "$m" ] || return 0
    n=$(printf '%s\n' "$m" | wc -l | tr -d ' ')
    hits=$((hits + n))
    echo "LEAK: $1: $n match(es)"
    [ "${LEAK_SHOW:-}" != 1 ] || printf '%s\n' "$m" | sort -u | sed 's/^/    /'
}

scan_file() { # FILE LABEL
    local f=$1 label=$2 x
    report "$label" <"$f"
    if tar -tf "$f" >/dev/null 2>&1; then
        tar -xOf "$f" 2>/dev/null | report "$label (unpacked)"
    elif unsquashfs -s "$f" >/dev/null 2>&1; then
        x=$(mktemp -d "$tmp/sq.XXXXXX")
        unsquashfs -q -n -f -d "$x/t" "$f" >/dev/null
        find "$x/t" -type f -exec cat {} + | report "$label (unpacked)"
        rm -rf "$x"
    fi
}

if [ "${1:-}" = --git ]; then
    cd "$root"
    n=0
    while IFS= read -r -d '' f; do
        [ -f "$f" ] || continue
        scan_file "$f" "$f"; n=$((n + 1))
    done < <(git ls-files -z -co --exclude-standard)
    o=0
    while read -r id type; do
        case $type in
            blob) git cat-file blob "$id" >"$tmp/obj"; scan_file "$tmp/obj" "git blob $id" ;;
            *) git cat-file -p "$id" | report "git $type $id" ;;
        esac
        o=$((o + 1))
    done < <(git cat-file --batch-all-objects --batch-check='%(objectname) %(objecttype)')
    what="$n files, $o git objects"
else
    [ $# -gt 0 ] || { echo "usage: $0 --git | PATH..." >&2; exit 2; }
    n=0
    for p in "$@"; do
        while IFS= read -r -d '' f; do scan_file "$f" "$f"; n=$((n + 1)); done < <(find "$p" -type f -print0)
    done
    what="$n files"
fi

if [ "$hits" -gt 0 ]; then
    echo "leakscan: $hits match(es) in $what; refusing" >&2
    exit 1
fi
echo "leakscan: clean ($what${LEAK_PATTERN:+, with local.env patterns})"
