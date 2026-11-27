#!/bin/sh
# Standalone / manual version of the Grok-region fix. Run it on the car (over SSH)
# after a reboot, only on a car WITHOUT CoilShift's built-in dev.coilshift.grok-region
# (that package is the durable form).
#   1) point car-assist at the EU (home) shard   2) open the client auth gate
# Safe to run anytime; only restarts car-assist if it isn't already on EU.
#
# Context: on an EU-homed VIN switched to US region, car-assist defaults to
# assistant-api.prd.na, and the NA shard refuses the interaction with QUIC
# application error 0x193 (= 403 Forbidden) -> "No Connection". Pointing it back
# at euw1 (where the VIN is entitled) + forcing MEDIA_needAuthToken=false makes
# Grok work airgapped, with no cloud token and Hermes closed.
export PATH=/usr/local/bin:/usr/bin:/bin:/sbin:/usr/sbin:$PATH

EP="assistant-api.prd.euw1.vn.cloud.tesla.com"

# 1) force the EU endpoint
mkdir -p /tmp/car-assist 2>/dev/null
rm -f /tmp/car-assist/force-na
touch /tmp/car-assist/force-eu

# 2) restart car-assist only if it's not already on EU
cur=""
for p in $(pgrep car-assist 2>/dev/null); do
  cur=$(tr '\0' '\n' < /proc/$p/cmdline 2>/dev/null | grep -m1 '^--endpoint=')
done
case "$cur" in
  *"$EP"*) echo "[=] car-assist already on EU ($cur)";;
  *) echo "[*] restarting car-assist -> EU"; sv restart car-assist; sleep 5;;
esac

# 3) open the client auth gate (home shard needs no live token)
sdv MEDIA_needAuthToken false

# 4) verify
echo "--- verify ---"
for p in $(pgrep car-assist 2>/dev/null); do tr '\0' ' ' < /proc/$p/cmdline 2>/dev/null; echo; done
echo "MEDIA_needAuthToken = $(lv MEDIA_needAuthToken 2>/dev/null)"
echo "[ok] now say 'Hey Grok' to test."
