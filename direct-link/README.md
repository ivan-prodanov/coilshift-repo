# direct-link (and direct-summon)

Today's working car side of the phone app, as two CoilShift packages, so it comes back by
itself at every boot instead of `install-car.sh --after-boot` over SSH.

- **Direct Link** (`io.github.ivan-prodanov.direct-link`): agw (the gateway, uid 4950) and
  camhelper (camera, talk, dashcam), plus the uid 4950 egress lock.
- **Direct Summon** (`io.github.ivan-prodanov.direct-summon`, folder `../direct-summon/`):
  summonhelper (uid 1337), which takes `com.tesla.Webrtc` from stock `webrtc-comms`.

Built from gateway `eccb2c4` (the car still runs `d91f0e1` from install-car.sh, feat/no-allowlist: no allowlist, first-connect pairing), and the same runit run, finish and
log scripts, apart from the binary paths (`test/run.sh` proves it against `install-car.sh`).
Design: [DESIGN.md](DESIGN.md). Plan: [PLAN.md](PLAN.md).

**Public repo.** This README may name car paths; never the VIN, the tailnet name or the phone key (those live in `local.env`).

## Status

Published 2026-11-27 (1.0.0, gateway `eccb2c4`) at `https://ivan-prodanov.github.io/coilshift-repo/`, by the owner's go-ahead.
**Not installed on the car yet** (migration below). The gateway's Go source has not reached Lupine yet (Author Agreement §3.2).

## What runs where on the car

| What | Where |
|---|---|
| Binaries | `/run/direct-link-bin/{agw,camhelper,BUILD}`, `/run/direct-summon-bin/{summonhelper,BUILD}` (the package images, bound) |
| runit service dirs | `/run/direct-link-sv/{agw,camhelper}`, `/run/direct-summon-sv/summonhelper` (each package's `$CSDK_RUN/sv`, bound) |
| Supervisor | each package's CoilShift service `supervise` runs its own `runsvdir -P $CSDK_RUN/svdir` |
| Keys, logs | as today: `/var/lib/airgapp-gw/{state,svlog}`, `/var/lib/airgapp-camhelper/svlog`, `/var/lib/airgapp-summon/state` |
| Config | `/var/lib/airgapp-gw/direct-link.conf`: `PI_IP` (default 192.168.4.1) and `HELPER_WRITER` (`pw-cat` or `aplay`) |

`direct-link.conf` is made at the first apply from today's installed scripts (so a migrated car
keeps its Pi address and writer), then it is yours to edit over SSH; it takes effect at the next
apply (a reboot, or an update). It is deliberately not a CoilShift setting.

The camera and Summon sign-in pin is chosen from the authd builds the sign-in pre-check proved
(`lib/gateway.sh`, `AUTHD_PINS`). An authd that is not in the table (a new firmware) gets no pin:
sign-in fails closed and the status says `unknown authd build`. Prove the new build with the
camera runbook's step 2b, add its hash in `build-payloads.sh`, rebuild, publish a new version.

## Build and test (this repo)

The gateway's Go source (agw, camhelper, summonhelper) is in [`../gateway/`](../gateway/), at the commit the
packages are built from (`eccb2c4`; added with `git subtree --squash` from the local gateway repo, update with
`git subtree pull --prefix=gateway <gateway repo> <branch> --squash`). The build uses only this repo.

```bash
direct-link/build-payloads.sh eccb2c4 <agw sha256> <camhelper sha256> <summonhelper sha256>
direct-link/test/run.sh
tools/build.sh
```

- `build-payloads.sh` builds the gateway commit with its own `make car` (go1.26.2, nothing
  downloaded), refuses unless the binaries are the reviewed ones, packs both images
  deterministically, scans them for leaks, and writes `lib/gateway.sh` and both manifests'
  `payloads`. The hashes are those of the reviewed commit's own `make car`.
- `test/run.sh`: the generated scripts equal `install-car.sh`'s for the same inputs (path map
  only); the shared files are identical in both packages; `bin/supervise.sh` on fakes (marker
  wait, stop order, runsvdir death, leftover runsvdir, take-it-back rate limit).
- `tools/build.sh`: lint, and the harness kinds of both `harness-seed.sh` files.
- Shared files (`lib/common.sh`, `lib/csdk.sh`, `lib/gateway.sh`, `bin/supervise.sh`): edit
  Direct Link's, then run `direct-link/sync.sh`.
- Every system tool the hooks and the supervisor call goes through `lib/common.sh`'s `tool`;
  under `CSDK_FAKE_ROOT` that is a fake from `test/fakes`, never the real one.

## Migration from install-car.sh (one time, owner at the car, over SSH)

Car parked, no Summon session open. Keep SSH output small.

1. Stop today's services, so their finish scripts run:
   `sv -w 15 down /var/lib/airgapp-gw/sv/summonhelper /var/lib/airgapp-gw/sv/camhelper /var/lib/airgapp-gw/sv/agw`
2. Stop today's runsvdir by its pid, checked by its exact cmdline:
   `for p in /proc/[0-9]*; do [ "$(tr '\0' ' ' <$p/cmdline 2>/dev/null)" = "runsvdir -P /var/lib/airgapp-gw/svdir " ] && echo ${p#/proc/}; done`,
   then `kill -HUP <that pid>`.
3. `rm /var/lib/airgapp-gw/svdir/*` and
   `mv /var/lib/airgapp-gw/install-car.sh /var/lib/airgapp-gw/install-car.sh.retired` (if it is
   there). Don't run `install-car.sh` again while the packages are installed. Leave stock
   `webrtc-comms` down: Direct Summon's apply finds it down.
4. In CoilShift: install Direct Link, then Direct Summon.
5. The checks below.

The keys are unchanged, so the phone needs nothing. The old binaries in
`/var/lib/airgapp-gw` and `/var/lib/airgapp-camhelper` stay unused; remove them by hand later.

## Checks after an install, an update or a reboot

- `cat /run/direct-link-bin/BUILD /run/direct-summon-bin/BUILD`: `build eccb2c4` in both.
- `sha256sum /run/direct-link-bin/agw /run/direct-link-bin/camhelper /run/direct-summon-bin/summonhelper`: the reviewed hashes.
- `sv status /run/direct-link-sv/* /run/direct-summon-sv/*`: all `run:`.
- `iptables -S OUTPUT | grep -m1 '^-A'`: the jump to `AIRGAPP_GW_OUT`.
- `tail -n 3 /var/lib/airgapp-gw/svlog/agw/current`: "bus self-test passed", "dialing the Pi".
- `grep -c 'live file /run/airgapp-summon/live' /var/lib/airgapp-gw/svlog/summonhelper/current`: 1 or more.
- Then with Summon open on the phone (no press): today's step 7 checks (presence lease on, the
  live file, its flock, SUMMON mode). No motion checks without the owner.

Also note on the first install (design Q1-Q9): does apply run at every boot and before the
service; do uids 4950 and 1337 exec from the binds; what signal and grace a stop gives
(`supervise` logs "stopping" in CoilShift's log); whether an update reruns apply in place.

## Removal

Remove Direct Summon (today's `SUMMON=0`): CoilShift stops `supervise` (summonhelper's finish
aborts and disconnects), then the hook waits for summonhelper to be gone and hands
`com.tesla.Webrtc` back to stock `webrtc-comms`. Removing Direct Link removes Direct Summon
first, then takes agw and camhelper down and removes the egress lock. Keys,
`direct-link.conf` and logs stay.
