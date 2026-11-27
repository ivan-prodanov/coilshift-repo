#!/bin/sh
# Sign-in pre-check before the install (runbook step 2b), and before any re-pin
# after an OTA. Car, as root, read-only: a temporary camhelper runs as root from
# the step 2 folder with the authd pin that matches this car's /usr/bin/authd, on
# its OWN state directory and sockets, and agw probe-auth asks it as uid 4950
# (no account, no bus). An installed camhelper is never touched: its state, its
# sockets (/run/airgapp-camhelper) and its service stay as they are, and the
# temporary one's start-up sweep only reads its own fresh state directory.
# Nothing is installed; the temporary camhelper is killed by its exact pid and
# every file it made is removed at the end. Uses agw and camhelper from runbook
# step 2, in the root-owned /tmp/airgapp-install.
#
#   ssh -p 13400 root@<car> "PHONE=<phone key hex> sh -s" < signin_precheck.sh > out.txt 2>&1
#
# Prints the authd build, then "owner=ok role=2 ..." for the phone key as uid
# 4950, then a refusal for uid 4952 at the socket directory.
umask 077
D=/tmp/airgapp-install
ST=/run/airgapp-signin-precheck
SOCKD=/run/airgapp-signin-precheck-sock
# The owner phone key (130 hex, uncompressed P-256), passed by the caller: never kept in this repo.
case ${PHONE:-} in 04*) ;; *) echo "set PHONE=<phone public key hex> (see the usage line)" >&2; exit 2 ;; esac
# The build installed next: the 2026-11-26 review fixes (gateway feat/summon-review-fixes 5dd87c6, camera + Summon;
# docs/SUMMON.md "Installing the 2026-11-26 review fixes"). The car ran 841d3df before it (agw 07cf4a48, camhelper
# d8cc4662).
WANT_AGW=f4ef404bc954f301984ecaff70875a0963375d8660d866c667fda301a206423e
WANT_CH=6c24978b153ade7fbc1733cf5aceab28969661df01e945e04833d77912ed9d6e
PIN_86=33d62d9e818afcb9b4a4af8cec6ecda2ff1801c5a667bd527e39a2b4f6f477e9
PIN_386=e0c74323da8f638b2d21d823cd989e0f6b43dfcf24b9db3e68aa9520057c1c0f
CHPID=
cleanup() {
	[ -n "$CHPID" ] && kill -TERM "$CHPID" 2>/dev/null && sleep 1
	[ -n "$CHPID" ] && kill -0 "$CHPID" 2>/dev/null && kill -KILL "$CHPID" 2>/dev/null
	rm -rf "$ST" "$SOCKD" "$D/precheck-ch.log" "$D/precheck-out"
	echo "cleanup: done (camhelper pid ${CHPID:-none} gone: $(kill -0 "${CHPID:-0}" 2>/dev/null && echo NO || echo yes))"
}

# Step 2's folder must be a real directory owned by root and writable by no one
# else, so no other uid can have planted or swapped anything in it.
[ -d "$D" ] && [ ! -L "$D" ] && [ -n "$(find "$D" -maxdepth 0 -type d -user 0 ! -perm -0020 ! -perm -0002 2>/dev/null)" ] ||
	{ echo "$D is not a root-owned directory writable only by root (run step 2 first); stop"; exit 1; }
[ "$(sha256sum "$D/agw" 2>/dev/null | cut -d' ' -f1)" = "$WANT_AGW" ] || { echo "HASH MISMATCH $D/agw (run step 2 first)"; exit 1; }
[ "$(sha256sum "$D/camhelper" 2>/dev/null | cut -d' ' -f1)" = "$WANT_CH" ] || { echo "HASH MISMATCH $D/camhelper (run step 2 first)"; exit 1; }
echo "hashes: ok"

# One pre-check at a time. Leftovers of an earlier, killed pre-check are ours to
# remove once no process uses them.
for _p in /proc/[0-9]*; do
	case "$(tr '\0' ' ' <"$_p/cmdline" 2>/dev/null)" in
	*" -state $ST "*) echo "another pre-check camhelper runs (pid ${_p##*/}); stop"; exit 1 ;;
	esac
done
rm -rf "$ST" "$SOCKD"
trap cleanup EXIT INT TERM

if [ -S /run/airgapp-camhelper/auth.sock ]; then
	echo "note: an installed camhelper serves /run/airgapp-camhelper; it is left alone (this check uses $SOCKD)"
fi

AUTHD=$(sha256sum /usr/bin/authd | cut -d' ' -f1)
case "$AUTHD" in
"$PIN_86") echo "authd: 2026.8.6 build (the compiled-in pin; install WITHOUT AUTHD_SHA256)"; PIN= ;;
"$PIN_386") echo "authd: 2026.38.6 build (install WITH AUTHD_SHA256=$PIN_386)"; PIN=$PIN_386 ;;
*) echo "authd: UNKNOWN build $AUTHD: KeyInfo not verified for it; stop"; exit 1 ;;
esac

setsid "$D/camhelper" run -state "$ST" -sock "$SOCKD/cam.sock" -auth-sock "$SOCKD/auth.sock" \
	-writer pw-cat ${PIN:+-authd-sha256 "$PIN"} > "$D/precheck-ch.log" 2>&1 < /dev/null &
CHPID=$!
echo "camhelper pid=$CHPID"
i=0; while [ $i -lt 20 ] && [ ! -S "$SOCKD/auth.sock" ]; do sleep 0.25; i=$((i+1)); done
ls -l "$SOCKD/" | awk 'NR>1{print $1, $3, $4, $NF}'
echo "== probe-auth as 4950, phone key (must be owner=ok role=2 or 3) =="
chpst -u :4950:4950 "$D/agw" probe-auth -auth-socket "$SOCKD/auth.sock" -pub "$PHONE" > "$D/precheck-out" 2>&1; rc=$?
tail -n 4 "$D/precheck-out"; rm -f "$D/precheck-out"
echo "rc=$rc"
echo "== probe-auth as uid 4952 (must be refused at the socket) =="
chpst -u :4952:4952 "$D/agw" probe-auth -auth-socket "$SOCKD/auth.sock" -pub "$PHONE" > "$D/precheck-out" 2>&1
tail -n 3 "$D/precheck-out"; rm -f "$D/precheck-out"
echo "== camhelper log (auth lines) =="
grep -E 'auth|sign-in|authd|watch|listen' "$D/precheck-ch.log" | tail -n 14 | cut -c1-200
