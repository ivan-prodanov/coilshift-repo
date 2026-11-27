#!/bin/bash
# sync.sh: copy the files Direct Link and Direct Summon share (Direct Link's are the originals).
# Edit them in direct-link's package only, then run this; direct-link/test/run.sh checks it.
set -euo pipefail
repo=$(cd "$(dirname "$0")/.." && pwd)
src=$repo/direct-link/coilshift/io.github.ivan-prodanov.direct-link
dst=$repo/direct-summon/coilshift/io.github.ivan-prodanov.direct-summon
mkdir -p "$dst/lib" "$dst/bin"
for f in lib/common.sh lib/csdk.sh lib/gateway.sh bin/supervise.sh; do
    [ -f "$src/$f" ] || continue
    cp -p "$src/$f" "$dst/$f"
done
