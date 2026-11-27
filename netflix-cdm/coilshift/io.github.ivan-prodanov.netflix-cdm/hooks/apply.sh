#!/bin/bash
# Netflix CDM fix (apply / late): make the theater-webapps Netflix tile play on 2026.8.6.
#
# ONLY FOR OLD FIRMWARE WHERE THE BROWSER IS TOO OLD. Step 0 reads the browser's own
# Widevine version; when it is already PAYLOAD_CDM or newer (2026.38.3.2 ships 4.10.3050.0)
# the hook takes down anything an earlier run left and exits 10 (unsupported), so it never
# downgrades a newer firmware's CDM.
#
# Netflix's "browser too old" (E109) is a Widevine DRM-VERSION gate, not a UA gate
# (proven: a clean honored Chrome/148 UA still gets E109). The stock browser ships
# Widevine CDM 4.10.2934.0; Netflix wants newer. We keep the stock Chrome/140 engine
# (a full 148 transplant has no audio + is unstable on 8.6). Every change goes through
# the car API, so the daemon's ledger undoes it on removal and nothing in the read-only
# rootfs or another package's tile is ever edited:
#   1) free the webapp profile partition if it is full (Netflix IndexedDB needs room),
#   2) loop-mount the newer CDM (4.10.3050.0) from the payload (PUT /v1/images) and
#      bind it at /run/netflix-cdm, the path verified on the car (PUT /v1/binds),
#   3) overlay the Netflix tile's config with a copy that has a clean Chrome UA,
#   4) overlay chromium-entertainment.vars with a copy whose minijail line binds the
#      newer CDM into the sandbox (PUT /v1/overlays for both),
#   5) drop pre-warmed Netflix procs so the next open launches fresh and picks it up.
#
# The binds live in /run and the root mount namespace, so this runs in the 'late'
# (after_restart) phase and re-runs every boot. Both overlay copies are made from what
# the target shows now by an idempotent edit, so a re-run over our own overlay gives the
# same bytes. Exits 0, or 10 (unsupported) on firmware that doesn't need it. bash 3.2
# (the car's). CSDK_FAKE_ROOT is the SDK test harness only; on a car it is empty.
set -eu
: "${CSDK_PACKAGE_DIR:?}" "${CSDK_RUN:?}"
. "$CSDK_PACKAGE_DIR/lib/csdk.sh"
R=${CSDK_FAKE_ROOT:-}

MNT=/run/netflix-cdm
CDM=/usr/lib/tesla-chromium/WidevineCdm
TILE=/usr/tesla/UI/assets/web-apps/dev-coilshift-theater-webapps.netflix/config
EVARS=/etc/sandbox.d/vars/chromium-entertainment.vars
UA='Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/148.0.0.0 Safari/537.36'
work=$CSDK_RUN/netflix-cdm
mkdir -p "$work"

# write_if_changed FILE: put stdin into FILE in place (same inode, so a live overlay of FILE
# shows it) only when the bytes differ.
write_if_changed() {
    local new="$work/.new"
    cat > "$new"
    if [ ! -f "$1" ] || ! cmp -s "$new" "$1"; then cat "$new" > "$1"; fi
    rm -f "$new"
}

# ver_lt A B: true when the dotted version A is lower than B.
ver_lt() {
    local a b i
    IFS=. read -r -a a <<< "$1"
    IFS=. read -r -a b <<< "$2"
    for i in 0 1 2 3; do
        [ "${a[$i]:-0}" -lt "${b[$i]:-0}" ] && return 0
        [ "${a[$i]:-0}" -gt "${b[$i]:-0}" ] && return 1
    done
    return 1
}

# 0) OLD FIRMWARE ONLY: go on only while the browser's own CDM is older than ours
PAYLOAD_CDM=4.10.3050.0
stock=$(sed -n 's/.*"version"[[:space:]]*:[[:space:]]*"\([0-9.]*\)".*/\1/p' "$R$CDM/manifest.json" 2>/dev/null | head -n 1)
if [ -z "$stock" ] || ! ver_lt "$stock" "$PAYLOAD_CDM"; then
    for route in /overlays/sandbox-vars /overlays/netflix-tile /binds/netflix-cdm /images/widevine; do
        csdk_api DELETE "$route" >/dev/null 2>&1 || true
    done
    if [ -z "$stock" ]; then
        reason="only for old firmware: can't read the browser's Widevine version at $CDM/manifest.json, so nothing was changed"
    else
        reason="not needed: this firmware's browser already has Widevine $stock (>= $PAYLOAD_CDM); only for old firmware where the browser is too old"
    fi
    csdk_api PUT /status "{\"state\":\"unsupported\",\"reason\":$(csdk_json_string "$reason")}" >/dev/null 2>&1 || true
    csdk_log info "netflix-cdm: $reason" || true
    exit 10
fi

# 1) free /opt/games/var if low (clears stale theater-apps.* duplicate caches)
free=$(df -Pk "$R/opt/games/var" 2>/dev/null | awk 'END{print $4}')
if [ "${free:-999999}" -lt 102400 ]; then
    rm -rf "$R"/opt/games/var/WebApp.theater-apps.* 2>/dev/null || true
    csdk_log info "freed /opt/games/var (cleared stale theater-apps.* caches)" || true
fi

# 2) the newer Widevine CDM: image from the payload, bound where the sandbox vars expect it
img=$(csdk_api PUT /images/widevine '{"payload":"payload/widevine-3050.sqfs"}' 2>&1) \
    || { csdk_log error "netflix-cdm: CDM image mount failed: $img" || true; exit 0; }
csdk_api PUT /binds/netflix-cdm "{\"source\":\"$CSDK_RUN/images/widevine\",\"target\":\"$MNT\"}" >/dev/null \
    || { csdk_log error "netflix-cdm: CDM bind at $MNT failed" || true; exit 0; }

# 3) tile UA: strip prior overrides, drop the TESLA_ prefix, advertise a clean Chrome UA
if [ -f "$R$TILE" ]; then
    { sed -e '/CHROMIUM_FLAGS+=("--user-agent=/d' \
          -e '/CHROMIUM_FLAGS+=("--remote-debugging-port=/d' \
          -e '/CHROMIUM_FLAGS+=("--remote-allow-origins=/d' \
          -e 's#CHROMIUM_CONFIG_USERAGENT_PREFIX="TESLA_"#CHROMIUM_CONFIG_USERAGENT_PREFIX=""#' "$R$TILE"
      printf 'CHROMIUM_FLAGS+=("--user-agent=%s")\n' "$UA"
    } | write_if_changed "$work/netflix.config"
    csdk_api PUT /overlays/netflix-tile "{\"target\":\"$TILE\",\"source\":\"$work/netflix.config\"}" >/dev/null \
        || csdk_log warn "netflix-cdm: tile overlay failed" || true
else
    csdk_log warn "netflix-cdm: $TILE not found (is theater-webapps installed?)" || true
fi

# 4) bind the newer CDM into the sandbox: drop any CDM bind we added before, then add ours
#    in front of the tesla-chromium bind (the order verified on the car)
sed -e "s# -b[^ ]*,$CDM##g" \
    -e "s#-b/usr/lib/tesla-chromium/tesla-chromium,/usr/lib/tesla-chromium/tesla-chromium#-b$MNT,$CDM -b/usr/lib/tesla-chromium/tesla-chromium,/usr/lib/tesla-chromium/tesla-chromium#" \
    "$R$EVARS" 2>/dev/null | write_if_changed "$work/chromium-entertainment.vars"
if grep -q -- "-b$MNT,$CDM " "$work/chromium-entertainment.vars"; then
    csdk_api PUT /overlays/sandbox-vars "{\"target\":\"$EVARS\",\"source\":\"$work/chromium-entertainment.vars\"}" >/dev/null \
        || csdk_log warn "netflix-cdm: sandbox vars overlay failed" || true
else
    csdk_log warn "netflix-cdm: launcher-bind anchor not found in $EVARS; CDM not bound into the sandbox" || true
fi

# 5) drop pre-warmed Netflix procs so the next open launches fresh (picks up the binds)
if [ -z "$R" ]; then
    for d in /proc/[0-9]*; do
        [ -r "$d/cmdline" ] || continue
        cmd=$(tr '\0' ' ' < "$d/cmdline" 2>/dev/null) || continue
        case "$cmd" in *dev-coilshift-theater-webapps.netflix*) kill -9 "${d#/proc/}" 2>/dev/null || true ;; esac
    done
fi

csdk_log info "netflix-cdm applied (Widevine 4.10.3050 bound into the Netflix sandbox; clean UA)" || true
exit 0
