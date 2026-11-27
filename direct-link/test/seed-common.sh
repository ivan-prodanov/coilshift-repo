# seed-common.sh ROOT: a car as today's install left it (sourced by both harness seeds).
# DUMP (default ../tesla-dump) gives the real 2026.38.6 authd, whose hash is a proven pin.
seed_car() {
    local root=$1 fakes dump
    fakes=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/fakes
    dump=${DUMP:-$(cd "$fakes/../../.." && pwd)/../tesla-dump}
    mkdir -p "$root/.fakes/state" "$root/proc/sys/kernel/random" "$root/proc/self" "$root/usr/bin" \
        "$root/etc/sv/webrtc-comms" "$root/var/lib/airgapp-gw/state" "$root/var/lib/airgapp-camhelper/svlog" \
        "$root/var/lib/airgapp-gw/svlog/agw" "$root/var/lib/airgapp-gw/svlog/summonhelper" \
        "$root/var/lib/airgapp-summon/state" "$root/var/etc"
    cp "$fakes"/* "$root/.fakes/"
    echo 6a1c0b5e-0000-4000-8000-000000000001 >"$root/proc/sys/kernel/random/boot_id"
    : >"$root/proc/self/mounts"
    cp "$dump/2026.38.6/usr/bin/authd" "$root/usr/bin/authd"
    echo up >"$root/.fakes/state/webrtc"
    echo true >"$root/.fakes/state/owner"
    echo '-A OUTPUT -m owner --uid-owner 1000 -j TESLA_EXAMPLE_OUT' >"$root/.fakes/state/iptables.rules"
    : >"$root/var/lib/airgapp-gw/state/key.pem"
    : >"$root/var/lib/airgapp-gw/allow"
    : >"$root/var/lib/airgapp-summon/state/key.pem"
    printf 'PI_IP=192.168.4.1\nHELPER_WRITER=pw-cat\n' >"$root/var/lib/airgapp-gw/direct-link.conf"
    for d in var/lib/airgapp-gw/svlog/agw var/lib/airgapp-gw/svlog/summonhelper var/lib/airgapp-camhelper/svlog; do
        printf 's1000000\nn3\n' >"$root/$d/config"; chmod 0750 "$root/$d"
    done
}
# fake_installer_runsvdir ROOT: today's install-car.sh supervisor, still running.
fake_installer_runsvdir() {
    mkdir -p "$1/proc/4242"
    printf 'runsvdir\0-P\0/var/lib/airgapp-gw/svdir\0' >"$1/proc/4242/cmdline"
}
