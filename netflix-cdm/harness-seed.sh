#!/bin/bash
# harness-seed.sh ROOT KIND: builds a csdk-harness fake root for the netflix-cdm package
# from the firmware dumps in ../tesla-dump. tools/build.sh runs it once per line of
# `harness-seed.sh --list` ("<kind> <expected outcome>") instead of a plain `csdk test`,
# which can't seed a root and so would never reach the real apply path.
#
#   old  2026.8.6 sandbox vars + Netflix tile, browser CDM 4.10.2934.0 (stock 2026.8.6;
#        the dump has no browser tree, so its manifest is written here): must apply cleanly
#   new  the same files with 2026.38.6's browser CDM manifest (4.10.3050.0; byte-identical
#        to 2026.38.3.2's, whose dump was retired 2026-11-21): the package is only for old
#        firmware, so it must change nothing and exit 10 (unsupported)
set -euo pipefail

if [ "${1:-}" = --list ]; then
    printf 'old pass\nnew unsupported\n'
    exit 0
fi

root=${1:?root}
kind=${2:?kind}
dump=${DUMP:-$(cd "$(dirname "$0")/../.." && pwd)/tesla-dump}
cdm=$root/usr/lib/tesla-chromium/WidevineCdm
tile=$root/usr/tesla/UI/assets/web-apps/dev-coilshift-theater-webapps.netflix

mkdir -p "$root/etc/sandbox.d/vars" "$tile" "$cdm"
cp "$dump/2026.8.6/etc/sandbox.d/vars/chromium-entertainment.vars" "$root/etc/sandbox.d/vars/"
cp "$dump/2026.8.6/usr/tesla/UI/assets/web-apps/dev-coilshift-theater-webapps.netflix/config" "$tile/"
case "$kind" in
    old) printf '{\n  "manifest_version": 2,\n  "name": "WidevineCdm",\n  "version": "4.10.2934.0"\n}\n' > "$cdm/manifest.json" ;;
    new) cp "$dump/2026.38.6/usr/lib/tesla-chromium/WidevineCdm/manifest.json" "$cdm/" ;;
    *) echo "unknown kind: $kind" >&2; exit 2 ;;
esac
