#!/bin/bash
# Fast VNC apply hook. CoilShift's SDK 1.0 only expands a package that has an apply hook or byte
# rules, so a service-only package needs one. CoilShift also runs it again when a setting changes:
# the service reads the password and picture size only when it starts, so restart it when they
# differ from what it started with. Safe to run twice.
set -eu
: "${CSDK_RUN:?}" "${CSDK_SETTINGS:?}" "${CSDK_PACKAGE_DIR:?}" "${CSDK_VERSION:?}"
. "$CSDK_PACKAGE_DIR/lib/csdk.sh"
. "$CSDK_PACKAGE_DIR/lib/settings.sh"

mkdir -p "$CSDK_RUN"
printf '%s\n' "$CSDK_VERSION" > "$CSDK_RUN/applied.new.$$"
mv -f "$CSDK_RUN/applied.new.$$" "$CSDK_RUN/applied"

used=$CSDK_RUN/settings.used   # written by bin/fastvnc.sh when it starts x11vnc
if [ -f "$used" ] && [ "$(cat "$used")" != "$(fv_settings_sum)" ]; then
    csdk_api POST /services/vnc/restart >/dev/null || csdk_log warn "settings changed, but the service did not restart" || true
fi
exit 0
