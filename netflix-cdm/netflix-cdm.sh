#!/bin/sh
# Standalone / manual version of the Netflix CDM fix. Run it ON THE CAR (over SSH)
# after a reboot (apply-all.sh does). ONLY FOR OLD FIRMWARE WHERE THE BROWSER IS TOO OLD
# (Widevine < 4.10.3050.0, e.g. 2026.8.6). Don't run it together with the
# io.github.ivan-prodanov.netflix-cdm package: they bind the same paths.
#
# Netflix's "browser too old" (E109) on 2026.8.6 is a Widevine DRM-VERSION gate (not a
# user-agent gate). The stock browser ships Widevine CDM 4.10.2934.0; Netflix wants
# newer. This keeps the stock Chrome/140 engine and only: binds a newer CDM
# (4.10.3050.0) into the Netflix sandbox, sets a clean Chrome UA on the tile, and frees
# the webapp profile partition. Everything is a bind or one tile line - the read-only
# rootfs is untouched - so RE-RUN this after each reboot (binds live in /run + the root
# mount namespace). Safe to re-run anytime.
#
# ONE-TIME SETUP: copy widevine-3050.sqfs to the car first (sshd here has no sftp):
#   ssh -p 13400 root@<car> 'mkdir -p /opt/games/usr/local/netflix-fix'
#   ssh -p 13400 root@<car> 'cat > /opt/games/usr/local/netflix-fix/widevine-3050.sqfs' \
#       < payloads/widevine-3050.sqfs
# (the file survives reboots on /opt/games/usr; only the binds below need re-applying.)
export PATH=/usr/local/bin:/usr/bin:/bin:/sbin:/usr/sbin:$PATH

IMG=/opt/games/usr/local/netflix-fix/widevine-3050.sqfs
MNT=/run/netflix-cdm
TILE=/usr/tesla/UI/assets/web-apps/dev-coilshift-theater-webapps.netflix/config
EVARS=/etc/sandbox.d/vars/chromium-entertainment.vars
WORK=/opt/games/usr/local/netflix-fix
UA='Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/148.0.0.0 Safari/537.36'

[ -f "$IMG" ] || { echo "[!] CDM image missing: $IMG  (do the one-time upload first - see header)"; exit 1; }

# 1) free /opt/games/var if low (stale theater-apps.* duplicate caches broke Netflix IndexedDB)
free=$(df -Pk /opt/games/var 2>/dev/null | awk 'END{print $4}')
if [ "${free:-999999}" -lt 102400 ]; then
  echo "[*] /opt/games/var low (${free}K) -> clearing stale theater-apps.* caches"
  rm -rf /opt/games/var/WebApp.theater-apps.* 2>/dev/null
fi
echo "[=] /opt/games/var free: $(df -Pk /opt/games/var | awk 'END{print int($4/1024)"M"}')"

# 2) loop-mount the newer Widevine CDM (4.10.3050)
if grep -q " $MNT " /proc/mounts; then
  echo "[=] CDM already mounted"
else
  mkdir -p "$MNT"
  mount -o loop,ro "$IMG" "$MNT" 2>/dev/null || { L=$(losetup -f); losetup "$L" "$IMG" && mount -t squashfs -o ro "$L" "$MNT"; }
fi
echo "[*] CDM mounted: $(grep -oE '4\.10\.[0-9.]+' "$MNT/manifest.json" | head -1)"

# 3) tile UA: strip prior overrides, drop TESLA_ prefix, advertise a clean modern Chrome
sed -i '/CHROMIUM_FLAGS+=("--user-agent=/d; /CHROMIUM_FLAGS+=("--remote-debugging-port=/d; /CHROMIUM_FLAGS+=("--remote-allow-origins=/d' "$TILE"
sed -i 's#CHROMIUM_CONFIG_USERAGENT_PREFIX="TESLA_"#CHROMIUM_CONFIG_USERAGENT_PREFIX=""#' "$TILE"
printf 'CHROMIUM_FLAGS+=("--user-agent=%s")\n' "$UA" >> "$TILE"
echo "[*] tile UA set to clean Chrome/148"

# 4) bind the newer CDM into the Netflix sandbox (chromium-entertainment.vars is read-only)
grep -q " $EVARS " /proc/mounts && umount "$EVARS" 2>/dev/null
[ -f "$WORK/chromium-entertainment.vars.orig" ] || cp "$EVARS" "$WORK/chromium-entertainment.vars.orig"
cp "$WORK/chromium-entertainment.vars.orig" "$WORK/chromium-entertainment.vars.patched"
sed -i "s#-b/usr/lib/tesla-chromium/tesla-chromium,/usr/lib/tesla-chromium/tesla-chromium#-b$MNT,/usr/lib/tesla-chromium/WidevineCdm -b/usr/lib/tesla-chromium/tesla-chromium,/usr/lib/tesla-chromium/tesla-chromium#" "$WORK/chromium-entertainment.vars.patched"
if grep -q "$MNT,/usr/lib/tesla-chromium/WidevineCdm" "$WORK/chromium-entertainment.vars.patched"; then
  mount --bind "$WORK/chromium-entertainment.vars.patched" "$EVARS"
  echo "[*] CDM bind active in chromium-entertainment sandbox"
else
  echo "[!] launcher-bind anchor not found in $EVARS - aborting"; exit 1
fi

# 5) drop pre-warmed Netflix procs so the next open launches fresh (picks up the bind)
n=0
for p in $(pgrep -f dev-coilshift-theater-webapps.netflix 2>/dev/null); do kill -9 "$p" 2>/dev/null && n=$((n+1)); done
echo "[*] cleared $n stale Netflix procs"

echo "[ok] done - open the Netflix tile and play. (verify: a fresh instance should show CDM 4.10.3050.0)"
