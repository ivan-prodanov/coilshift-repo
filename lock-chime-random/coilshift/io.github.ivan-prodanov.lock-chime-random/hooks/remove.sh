#!/bin/bash
# remove hook: restore the stock lock sound. The service is stopped by the supervisor (TERM),
# and its teardown already restores from CSDK_STATE; this is the belt-and-suspenders restore for
# package removal, and it leaves the backup in place. It first stops any watcher an earlier
# service run left behind (see lr_reap_stale), so nothing swaps a clip in after the restore.
set -u
: "${CSDK_STATE:?}"
pkg=${CSDK_PACKAGE_DIR:-$(cd "$(dirname "$0")/.." && pwd)}
. "$pkg/lib/csdk.sh"
. "$pkg/lib/lockrand.sh"

BACKUP="$CSDK_STATE/LockChime.orig.wav"
lr_reap_stale "$CSDK_STATE/lockrand.pids"
rm -f "$CSDK_STATE/lockrand.pids" "$CSDK_STATE/lockrand.events" 2>/dev/null
if [ -f "$BACKUP" ]; then
    lr_install "$BACKUP" && echo "lock-chime-random: restored stock lock sound"
fi
exit 0
