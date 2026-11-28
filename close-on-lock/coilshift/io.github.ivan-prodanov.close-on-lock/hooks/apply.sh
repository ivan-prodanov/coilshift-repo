#!/bin/bash
# apply hook: records the version this run applied, nothing else. The package's work is all in
# the service. The hook exists because the car's CoilShift only expands a package that has an
# apply hook or byte rules. 1.0.0 had neither and was refused ("does not expand: ... has no
# apply hook and no byte rules, the only things SDK 1.0 runs"). csdk lint and csdk test did not
# catch it. Safe to run twice.
set -eu
: "${CSDK_RUN:?}" "${CSDK_VERSION:?}"
mkdir -p "$CSDK_RUN"
printf '%s\n' "$CSDK_VERSION" > "$CSDK_RUN/applied.new.$$"
mv -f "$CSDK_RUN/applied.new.$$" "$CSDK_RUN/applied"
exit 0
