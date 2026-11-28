#!/bin/bash
# Fast VNC remove: stop any x11vnc still running from this package's copy, then delete the copy,
# the password file and the bookkeeping files. CoilShift stops the service first; this catches a leftover.
set -eu
: "${CSDK_RUN:?}"
R=${CSDK_FAKE_ROOT:-}
bin=$CSDK_RUN/bin/x11vnc

ours() {
    local dir found=''
    for dir in "$R"/proc/[0-9]*; do
        [ -d "$dir" ] || continue
        [ "$(readlink "$dir/exe" 2>/dev/null)" = "$bin" ] && found="$found ${dir##*/}"
    done
    printf '%s' "$found"
}
for pid in $(ours); do kill -TERM "$pid" 2>/dev/null || true; done
n=0
while [ -n "$(ours)" ] && [ "$n" -lt 10 ]; do sleep 0.5; n=$((n + 1)); done
for pid in $(ours); do kill -KILL "$pid" 2>/dev/null || true; done

rm -f "$bin" "$bin.new" "$CSDK_RUN/passwd" "$CSDK_RUN/passwd.new" "$CSDK_RUN/settings.used" "$CSDK_RUN/applied"
rmdir "$CSDK_RUN/bin" 2>/dev/null || true
exit 0
