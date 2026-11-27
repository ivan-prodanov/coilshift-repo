#!/bin/bash
# apply hook: back up the stock lock sound (once) to CSDK_STATE so the service / remove.sh can
# restore it, and put the driver's next setup step in the package status (lib/lockrand.sh).
# Writes no rootfs; the service owns the swapping.
set -eu
: "${CSDK_PACKAGE_DIR:?}" "${CSDK_STATE:?}"
. "$CSDK_PACKAGE_DIR/lib/csdk.sh"
. "$CSDK_PACKAGE_DIR/lib/lockrand.sh"

BACKUP="$CSDK_STATE/LockChime.orig.wav"

# Back up the stock sound once. Only capture it when no backup exists yet, so we never overwrite
# the real stock sound with a random clip the service already staged. No LockChime.wav yet is
# not fatal: the status says how to pick a USB lock sound, and the service picks it up later.
if [ ! -f "$BACKUP" ] && [ -f "$LR_TARGET" ]; then
    if ! cp "$LR_TARGET" "$BACKUP"; then
        csdk_api PUT /status "{\"state\":\"degraded\",\"reason\":$(csdk_json_string "could not back up $LR_TARGET")}" >/dev/null 2>&1 || true
        exit 11
    fi
fi

lr_status
exit 0
