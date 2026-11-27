# Direct Link and Direct Summon: the car side as CoilShift packages (design, draft 2026-11-26)

**Status.** Draft for the owner's approval. Nothing here is built or published.

**Superseded in part by direct mode (1.1.0, 2026-11-27).** agw no longer dials a Pi: it listens on the car's own
tailnet address (`serve -tailnet`), the egress lock allows only agw's replies on `tailscale0` from port 17100, and
`direct-link.conf` has no `PI_IP`. Everything below about `PI_IP`, `-dial` and port 17101 describes 1.0.x. See
[README.md](README.md), "Direct mode".

**Intent.** The owner asked on 2026-11-26 whether the car side "in its entirety" can be a CoilShift (CS) package. That is the
gateway (agw), the camera helper (camhelper), the Summon helper (summonhelper) and the per-boot setup. Today:
- these are installed over SSH with `deploy/install-car.sh` from the gateway repo (`~/Work/airgapp/gateway`, local only);
- after every real MCU reboot the owner re-runs `install-car.sh --after-boot` by hand.

As CS packages they install and update like the owner's other packages, and CoilShift's daemon brings them back at every boot.

**Owner decisions behind this design (2026-11-26).**
- **The 2026-11-25 rule is dropped.** That rule said "our services never run under CoilShift's supervisor". The owner: "Forget my earlier
  rule."
- **Publishing on CoilShift's terms.** The owner said "Yes, Design it!" after reading the cost:
  - binaries downloadable by anyone with the repository URL;
  - the Author Agreement, under which Lupine gets the complete source of every version and may replace, roll back or block a package
    without notice.

  **Confirm both again before the first publish**: the gateway's Go source goes to Lupine with it.
- **Unattended start at every boot.** This includes stopping Tesla's stock `webrtc-comms` at every boot with no typed yes. The owner did
  not object when told; confirm with the plan.
- **"Take it back."** Summon re-takes `com.tesla.Webrtc` whenever it finds stock holding it while the package is installed, not only at
  boot. This covers a CoilShift restart of the service, or stock coming back in between. The owner chose this over "off until reboot".
- **Same conventions as the other packages.** Source in this repo, built packages in its `gh-pages`
  branch (the sources were private in `tesla-fw-patches` until 2026-11-27; now public, scrubbed).
- **Names: Direct Link and Direct Summon.** The owner dropped "airgapp" for the packages and narrowed the choice to `direct-*`
  or `offline-*`. `direct-*` is the designer's call. The phone reaches the Pi over the internet, so "offline" would mislead;
  "direct" says what is special, that no Tesla server sits between the phone and the car. For names on the car, see "Where
  things live on the car".

Background analysis: `tesla-dump/docs/coilshift-package-fit-2026-11-26.md`, with its addendum. SDK: `mLupine/coilshift-zero-sdk`
docs, csdk 0.1.2.

## Goals

- **G1.** After a reboot the car side comes back by itself: egress lock, binaries, services, Summon's takeover of `com.tesla.Webrtc`.
  No SSH step.
- **G2.** Install, update and removal are CoilShift actions: the owner taps. Removing Direct Summon is today's `SUMMON=0`:
  - Summon goes;
  - Tesla's stock `webrtc-comms` comes back;
  - the camera stays.
- **G3.** The car behaves exactly as with today's reviewed install, because the same binaries run the same runit run, finish and log
  scripts:
  - Summon motion: deadman D2, floors D3, H1, H4, the takeover;
  - the camera and its SUMMON mode;
  - talk, dashcam, logs.
- **G4.** Only the published build runs. Every binary is checked against hashes pinned in the package before anything starts.
- **G5.** Nothing secret is published: no keys, no pins, no VIN, no tailnet name, no `/Users` path.

## Non-goals

- The Pi. It is unchanged. (The phone changes only for first-connect pairing, below.)
- Provisioning a phone. (Changed 2026-11-26, owner: no allowlist and no SSH. The car admits any key authd lists as OWNER or DRIVER, and
  the phone trusts the car's key on first connect through HELLO/CHALLENGE v2: gateway branch feat/no-allowlist.) The package must never
  carry them.
- Making `install-car.sh` obsolete. It stays as the lab and fallback path. While the packages are installed it refuses, see "Gateway
  repo changes".

## Anti-goals

These would be failures even if everything "worked":
- a CS setting anywhere on the drive path. Settings can be changed from the app or the admin panel at any time;
- our services running without the egress lock;
- two supervisors (the installer's and the package's) running the same services;
- stock `webrtc-comms` left stopped with nothing owning the name after a removal or a skipped Summon.

## Constraints

**From the SDK (csdk 0.1.2 docs):**
- **Everything runs as root.** Hooks and services run as root, in CoilShift's cgroups, and the car API answers uid 0 only. The uid
  separation stays ours, as today: `chpst -u :4950:4950` for agw and `chpst -u :1337:1337` for summonhelper.
- **Hooks.** An apply hook (phase early or late, timeout 1-3600 s, exit 0 ok / 10 unsupported / 11 refused / 12 deferred /
  other failed), a remove hook, and up to 16 services (restart always, on-failure or never). There is no hook that runs when a service
  exits.
- **Where code can run.** The package tree is on noexec /var. `$CSDK_RUN` (`/run/coilshift/pkgs/<id>`) is boot-scoped, executable and
  root-only. A payload image is loop-mounted read-only at `$CSDK_RUN/images/<name>`. Observed on the car on 2026-11-12: image mounts
  are `ro,nosuid,nodev` and not noexec. A non-root uid reaches image content only through a bind (`PUT /v1/binds`).
- **Publishing.**
  - The archive (the package folder) is published as full source.
  - Payloads are public under `pool/`, at most 32 per package, 64 MiB each.
  - Lupine's server fetches the repository, so it must be public HTTPS.
  - Updates install on the owner's tap. A rollback is a new, higher version.
- **Lint.** `csdk lint --strict` warns on raw `sv`, `iptables`, `kill` and `mount`, and the repo's build fails on a warning unless
  the package is in `tools/build.sh`'s `allow_warnings`.
- **The harness** runs the apply and remove hooks against a fake root. It never starts services and never runs iptables.

**From the project (gateway runbook `docs/SUMMON.md`, the owner's standing rules):**
- **Order.** The egress lock (uid 4950 may only reach TCP 192.168.4.1:17101 on wlan0; the jump is first in OUTPUT; IPv4 and IPv6)
  must exist before agw starts.
- **Summon's takeover order.** Stock `webrtc-comms` is down and the name is free before summonhelper starts. Removal hands the name
  back.
- **Killing.** Never kill a car process by name. Kill only by a pid read from a file and checked by its `comm`.
- **R22.** Nothing is ever sent to 192.168.90.0/24.
- **The authd pin.** It comes from builds proven by the sign-in pre-check (`deploy/signin_precheck.sh`), never from the car's own
  hash.
- **Motion.** Motion tests stay owner-only at the car.

## Approaches

1. **Our own supervisor inside CoilShift** (chosen). Each package has one CS service, a small bash wrapper that runs today's runit
   (`runsvdir`, `runsv`, the same generated run, finish and log scripts). The apply hook does what `--after-boot` does today,
   unattended.
   - **For:** keeps G3 exactly. Summonhelper's `finish` still sends cmd_abort and the disconnect after any exit of its supervisor.
     The svlogd logs and the runbook's `sv` checks still work.
   - **Against:** two supervisors nested. Raw `sv`, `iptables` and `kill` need the lint allow-list.
2. **CoilShift supervises each program directly** (three CS services with `chpst` wrappers).
   - **For:** the native shape.
   - **Against:** no runit finish. A crash of summonhelper's supervisor would wait on CoilShift's undocumented restart delay before
     the next start's takeover. The scripts are new and unreviewed, the logs move, and the runbook needs rewriting.
3. **A thin launcher only.** The binaries are still installed over SSH, and the package just runs an unattended `--after-boot`.
   - **For:** nothing becomes public.
   - **Against:** it is not "the whole car side". Two update channels remain.

## Design

### Packages and feature folders

Two feature folders, one per package, like every other package in this repo. `tools/build.sh` reads a package's payloads and
harness seed from the feature folder two levels above the package, so each folder has its own.

```
direct-link/
  README.md          owner runbook for both packages: migration, checks, provisioning (private: may name car paths;
                     never the VIN or tailnet name)
  DESIGN.md          this file, for both packages
  PLAN.md            the build plan (next step)
  build-payloads.sh  builds both payload images from one gateway commit (below)
  harness-seed.sh    fake-root seeds for csdk test, and the fake tools both seeds use
  payloads/          direct-link-<build>.sqfs (committed, like netflix-cdm's image)
  coilshift/io.github.ivan-prodanov.direct-link/      agw + camhelper (the camera, talk, dashcam)
    coilshift.json  hooks/apply.sh  hooks/remove.sh  bin/supervise.sh  lib/csdk.sh  lib/gateway.sh  lib/templates.sh
direct-summon/
  README.md          what the package does; points to direct-link/README.md
  harness-seed.sh    the Summon kinds, using direct-link's fake tools
  payloads/          direct-summon-<build>.sqfs
  coilshift/io.github.ivan-prodanov.direct-summon/    summonhelper
    coilshift.json  hooks/apply.sh  hooks/remove.sh  bin/supervise.sh  lib/csdk.sh  lib/gateway.sh  lib/templates.sh
```

Each package is self-contained. `lib/gateway.sh` holds the gateway build's binary hashes and the authd pin table:
`build-payloads.sh` writes the same file into both packages, so the copies can't drift. `lib/templates.sh` holds each package's own
run, finish and log scripts.

**IDs, titles and versions.**
- `io.github.ivan-prodanov.direct-link`, titled "Direct Link", and `io.github.ivan-prodanov.direct-summon`, titled "Direct Summon":
  short titles, like the other packages'. Both start at 1.0.0.
- Both bump together on every gateway build, and each records the build id: the short gateway commit, for example `d91f0e1`.
- Direct Summon has `requires.packages: ["io.github.ivan-prodanov.direct-link"]` and `hooks.apply.after` naming it.
- SDK 1.0 has no version ranges, so Direct Summon's apply refuses a mismatched build itself (below).

**Capabilities declared:** `service`, `image.mount`, `bind`. The raw `sv`, `iptables` and `kill` calls are disclosed in the summary,
as the Author Agreement requires: the package must not do anything its manifest and description don't disclose.

**Summaries** are plain-language, and each also discloses its raw calls:
- Direct Link: the live camera, talk through the car's external speaker and dashcam clips on the owner's phone, without Tesla's
  servers.
- Direct Summon: Summon from the owner's phone, without Tesla's servers. It says that the car drives only on the phone's command,
  and that Tesla's stock WebRTC service is stopped while the package is installed.

### Payload images

`direct-link/build-payloads.sh <gateway-commit>`:
1. `git archive`s that commit of the gateway repo into a temp dir.
2. Runs `make car` with the pinned toolchain (go1.26.2).
3. Checks the three binaries' sha256 against the values the builder passes in, which are the reviewed ones. For d91f0e1:
   - agw `f4ef404b…`
   - camhelper `6c24978b…`
   - summonhelper `8c16d153…`
4. Packs two images deterministically:
   `mksquashfs <dir> <out> -comp gzip -all-root -noappend -no-xattrs -mkfs-time 0 -all-time 0`.
   - `direct-link/payloads/direct-link-<build>.sqfs`: `agw`, `camhelper` and `BUILD`. BUILD holds the build id, the toolchain
     and the binaries' sha256.
   - `direct-summon/payloads/direct-summon-<build>.sqfs`: `summonhelper` and the same `BUILD`.
5. Writes each image's sha256 and size into its manifest's `payloads`, and the binary hashes into both packages'
   `lib/gateway.sh`.

Today's binaries are about 27 MB together, well under 64 MiB per image. `tools/publish.sh` cannot see inside squashfs. The build
therefore adds a payload scan: `unsquashfs` to a temp dir, then the same leak patterns over files and `strings`. The binaries read the
VIN at run time and embed no car identity, so the scan must come back clean.

### Where things live on the car

| What | Where | Notes |
|---|---|---|
| Binaries | `/run/direct-link-bin/{agw,camhelper,BUILD}`, `/run/direct-summon-bin/{summonhelper,BUILD}` | Image mounts bound out of `$CSDK_RUN/images/*` with `PUT /v1/binds`. Read-only, executable, reachable by uids 4950 and 1337 (checked at apply; see Q3). |
| Service dirs and svdir | `$CSDK_RUN/sv/<svc>/{run,finish,log/run}`, `$CSDK_RUN/svdir/<svc>` links | Rewritten every boot by apply. Root-only, executable, boot-scoped. Bound read-only to `/run/direct-link-sv` (Direct Link) and `/run/direct-summon-sv` (Direct Summon), so runbook commands have stable paths. |
| Keys, allowlist, config, logs | `/var/lib/airgapp-gw/{state/key.pem (4950 0400), direct-link.conf (root 0644), svlog/{agw,camhelper,summonhelper}}` | Persistent, kept from today's install. Data only, so noexec /var is fine and the exec bind mounts of today are no longer needed. |
| Summon key | `/var/lib/airgapp-summon/state` (1337 0700) | Compiled into summonhelper. Unchanged. |
| Runtime | `/run/airgapp-summon` (1337:4950 2750), `/run/airgapp-camhelper*` | Made by the run scripts, as today. |

**Names on the car.** Everything these packages add is named after them: `/run/direct-link-bin`, `/run/direct-summon-bin`,
`/run/direct-link-sv`, `/run/direct-summon-sv`, the two images and `direct-link.conf`. Names that the gateway binaries or today's
reviewed scripts already use keep their airgapp names: `/var/lib/airgapp-gw`, `/var/lib/airgapp-summon`, `/run/airgapp-summon`,
`/run/airgapp-camhelper*` and the chain `AIRGAPP_GW_OUT`. Renaming those needs a gateway release and moving the keys on the car,
so it waits for a reason. Until then the public hook scripts show them.

**`direct-link.conf`** holds `PI_IP` (default `192.168.4.1`) and `HELPER_WRITER` (`pw-cat` or `aplay`, default `pw-cat`):
- It is created with the defaults if it is missing.
- It is validated at every apply: `check_ip`, and the writer must be `pw-cat` or `aplay`.
- The owner edits it over SSH.
- It is deliberately not a CS setting (anti-goal 1). It also fixes today's quirk where a plain `--after-boot` resets an aplay car to
  pw-cat.

**The authd pin** comes from a table in `lib/gateway.sh` of builds the sign-in pre-check proved:
- 2026.38.6 `e0c74323…`
- 2026.8.6 `33d62d9e…`

Apply hashes `/usr/bin/authd`:
- if the hash is in the table, the camhelper and summonhelper run scripts get that pin;
- if not, they get no pin, so the compiled-in pin fails sign-in closed, and the status is degraded: "unknown authd build: run the
  pre-check, then a new package version adds it".

### Apply hook: `io.github.ivan-prodanov.direct-link`

Phase late, `rerun: in-place`, timeout 300 s. Every step is idempotent, and `apply,apply` equals one apply. It starts with a clean
environment, `set -eu`, and never prints a secret.

1. **Refuse double supervision.** If a process runs with the exact cmdline `runsvdir -P /var/lib/airgapp-gw/svdir`, the
   installer-managed setup is still live. Report "migrate first (README)" and exit 11.
2. **Mount and check.** `PUT /v1/images/direct-link` with the payload. Check `agw` and `camhelper` against `lib/gateway.sh`; on a
   mismatch, exit 11. Then `PUT /v1/binds` to `/run/direct-link-bin`, and confirm `chpst -u :4950:4950 test -x /run/direct-link-bin/agw`.
3. **First-run provisioning**, only what is missing, exactly as install steps 1, 3 and 4:
   - `/var/lib/airgapp-gw` (root 0755);
   - `state/` (4950:4950 0700);
   - `direct-link.conf`;
   - the svlog dirs;
   - the key, made with `chpst -u :4950:4950 /run/direct-link-bin/agw keygen -key …`.

   Nothing that exists is touched.
4. **The egress lock.** Today's chain `AIRGAPP_GW_OUT`, with raw `iptables`/`ip6tables`:
   - IPv4: ACCEPT TCP to `PI_IP:17101` on wlan0, then DROP;
   - IPv6: DROP;
   - the owner jump is first in OUTPUT.

   It is re-asserted every apply and checked with today's `egress_chain_ok` and `egress_jump_first`. On failure the status is
   failed; exit with a failure and write no agw service. agw's run script also refuses without the lock, as today.
5. **Legacy.** The 2026-11-20 `/etc/passwd` overlay is only checked and reported. Mounts don't survive a reboot.
6. **Service scripts.** Write the agw and camhelper run, finish and log scripts to `$CSDK_RUN/sv` from `lib/templates.sh`, and link
   them into `$CSDK_RUN/svdir`.
   - The templates are today's S2 templates with the binary paths changed, the same preconditions inside: the egress-lock check,
     `-dial PI_IP:17101`, the writer and the pin. camhelper's finish is still `camhelper sweep`.
   - `PUT /v1/binds` `$CSDK_RUN/sv` to `/run/direct-link-sv`.
7. **Finish up.** Write `$CSDK_RUN/applied` with the build id and the boot id (`/proc/sys/kernel/random/boot_id`). `PUT /v1/status`:
   `ok`, or degraded with a reason of at most 256 bytes. On a rerun where the service already runs (an update), restart it:
   `POST /v1/services/supervise/restart`.

### Apply hook: `io.github.ivan-prodanov.direct-summon`

Phase late, after Direct Link, `rerun: in-place`, timeout 300 s.

1. **The same refusal** as Direct Link's step 1.
2. **Build match.** `/run/direct-link-bin/BUILD`'s build id must equal this package's own, or exit 11 with "Direct Link and Direct Summon
   are different builds". A mismatched pair would fail closed anyway: camhelper refuses SUMMON mode.
3. **Mount and check** the Summon image, bind it to `/run/direct-summon-bin`, and confirm `chpst -u :1337:1337 test -x`.
4. **Provisioning**, only if missing: `/var/lib/airgapp-summon` (root 0755), `state/` (1337 0700), and the key made as uid 1337.
5. **The takeover (S0b), unattended.** If our summonhelper is already supervised and running (a rerun), skip this step. Otherwise:
   - `sv down /etc/sv/webrtc-comms`;
   - SIGKILL the orphaned `minijail-init` only by the pid in `/run/service/webrtc-comms/webrtc-comms.pid`, after checking its
     `comm`, then remove the pidfile;
   - wait up to 60 s for `com.tesla.Webrtc` to have no owner (`gdbus` NameHasOwner, each call under `timeout -k 1 5`).

   If the name stays taken:
   - hand stock back if we stopped it (today's `stock_hand_back`);
   - write no Summon service;
   - set the status to degraded with "Summon skipped: com.tesla.Webrtc stayed taken";
   - exit 0, since the camera is unaffected.
6. **Service scripts.** Write summonhelper's run, finish and log scripts from today's templates into this package's `$CSDK_RUN/sv`
   (bound to `/run/direct-summon-sv`). The run script, unchanged:
   - prepares `/run/airgapp-summon` (1337:4950 2750);
   - removes a stale `relay.sock`, `live` and `.live.*`;
   - reads the VIN;
   - refuses while the name has an owner;
   - then runs `chpst -u :1337:1337 /run/direct-summon-bin/summonhelper run …`.

   The finish script stays `chpst -u :1337:1337 … summonhelper finish`.
7. **Finish up.** The marker, the status, and the restart on an update, as in Direct Link's step 7.

### The service: `bin/supervise.sh` (one per package, `restart: always`)

1. Wait until `$CSDK_RUN/applied` exists for this boot. Re-check every second; after 10 minutes, report degraded once and keep
   waiting. This is what makes the order hold (lock and takeover before any start), whatever order CoilShift starts things in.
2. Refuse to start a second `runsvdir` on the same svdir: check by pid file plus cmdline. Then start `runsvdir -P $CSDK_RUN/svdir` in
   the background and wait on it.
3. On TERM, INT or HUP:
   - for each service, `sv -w 15 down`, Summon first. That runs the finish scripts: summonhelper's cmd_abort and disconnect,
     camhelper's sweep;
   - then `kill -HUP` our runsvdir (by its pid), which stops every runsv;
   - wait, then exit 0.
4. If runsvdir dies on its own, exit non-zero, so CoilShift restarts the wrapper. A second runsv on the same dir cannot take
   `supervise/lock`, so nothing doubles.
5. **Summon only: take it back.** This is the owner's choice. In two places the wrapper runs the same bounded takeover as apply step
   5: `sv down` stock, the checked-pidfile `minijail-init` cleanup, then wait for the name.
   - **At start:** before it starts runsvdir, if `com.tesla.Webrtc` has an owner and no summonhelper of ours runs.
   - **While running:** every 10 s, if summonhelper is not running and the name has an owner. This is the case where summonhelper's
     run script keeps refusing because stock took the name in between, for example after a CoilShift restart.

   Limits and reporting:
   - at most once a minute;
   - each attempt is logged to `apply.log`;
   - a name that stays taken sets the status to degraded, with "Summon waiting: com.tesla.Webrtc taken".

   The reviewed run script is unchanged and still refuses while the name has an owner.

### Remove hooks

Both are idempotent and work whichever way the docs' conflicting order of "stop services" versus "run the hook" turns out. A failure
halts the removal, which CoilShift then retries.

**Direct Summon:**
1. `sv -w 15 down` summonhelper, if its service dir exists (its finish runs).
2. Wait up to 10 s until no process runs with the exact path `/run/direct-summon-bin/summonhelper`. If one lingers: fail, leave
   stock alone.
3. Remove `/run/airgapp-summon/live` and `.live.*`.
4. Hand `com.tesla.Webrtc` back to stock with today's `stock_hand_back`:
   - `sv -w 15 down`;
   - the orphaned `minijail-init` by its checked pidfile;
   - `sv up`;
   - wait up to 60 s for registration.
5. Keep `/var/lib/airgapp-summon/state`.

**Direct Link:**
1. `sv -w 15 down` camhelper and agw. camhelper's finish sweeps its keep-awake latches.
2. Once no agw process runs, remove the egress lock.
3. Keep keys, config and logs.

CoilShift's ledger then undoes the binds and image mounts. If Direct Link is removed, Direct Summon goes with it (its
dependency), and its remove hook needs nothing from Direct Link.

### Updates and rollback

- **A new gateway build means both packages bump together.** The owner publishes, then taps update while parked.
- **What an in-place apply does:**
  1. re-`PUT`s the image: the old mount detaches lazily, and running processes keep the old binaries until they restart;
  2. re-`PUT`s the binds;
  3. rewrites the scripts;
  4. restarts the service through the car API. The wrapper's TERM path runs `sv down`, so each finish runs, before the new
     binaries start.
- **Rollback** is a higher version that carries the old payload.
- **Publishing always pairs builds.** The build publishes both packages together and refuses a release where their build ids
  differ.

### Migration from today's install (one time, owner, over SSH)

This goes in `direct-link/README.md`:
1. With no Summon session open: `sv -w 15 down` summonhelper, agw and camhelper through `/var/lib/airgapp-gw/sv/*`. Their finish
   scripts run.
2. Stop the installer's runsvdir by its pid, checked by its exact cmdline: `kill -HUP`.
3. Remove `/var/lib/airgapp-gw/svdir/*` and rename `/var/lib/airgapp-gw/install-car.sh` to `install-car.sh.retired`. Leave stock
   `webrtc-comms` down: Summon's apply finds it down.
4. In CoilShift, install Direct Link, then Direct Summon. The repository is already listed.
5. Run the checks below.

The old binaries in `/var/lib/airgapp-gw` and `/var/lib/airgapp-camhelper` stay unused, and are removed later by hand. The keys,
pins are unchanged, so the phone needs nothing.

### Checks after an install or update

These adapt `docs/SUMMON.md`'s install checks to the new paths. Keep every output small:
- `cat /run/direct-link-bin/BUILD` and `cat /run/direct-summon-bin/BUILD`: the expected build id.
- `sha256sum` of the three binaries: the reviewed hashes.
- `sv status /run/direct-link-sv/* /run/direct-summon-sv/*`: all `run:`.
- `iptables -S OUTPUT | head -1`: the jump to AIRGAPP_GW_OUT.
- agw's log shows "bus self-test passed" and "dialing the Pi".
- summonhelper's log shows `summonhelper: live file /run/airgapp-summon/live`.
- Then, with Summon open on the phone, today's step 7 checks: `presence lease on`, the live file, the flock and SUMMON mode.
- Then today's no-motion checks (owner-only).

### Status and logs

- **Status.** `PUT /v1/status` with a generic state and reason only: no VIN, keys, pins, tailnet or session data.
- **Logs.** svlogd keeps writing to `/var/lib/airgapp-gw/svlog/*`, 3 files of 1 MB each. The apply hooks also write a short line per
  step to `/var/lib/airgapp-gw/svlog/apply.log`, which is kept small.

### Build, lint and harness tests (this repo)

- **`tools/build.sh`.** Add `direct-link` and `direct-summon` to `allow_warnings`, with a comment naming the accepted warnings: raw `sv`,
  `iptables`/`ip6tables`, `kill` by checked pid, `runsvdir`, `gdbus`. Add the payload leak scan.
- **`harness-seed.sh`**, one per feature folder. Each seeds a fake root: `/var/lib/airgapp-*`, `/etc/sv/webrtc-comms`, the pidfile and `/usr/bin/authd`.
  Every raw command goes through one wrapper function per tool (`do_sv`, `do_iptables`, `do_kill`, `do_gdbus`), and under
  `CSDK_FAKE_ROOT` those call fakes from the seed and never the real tool. `kill` only ever targets a pid read from a file inside the
  fake root. Host safety is non-negotiable: two earlier test harnesses nearly signalled real pids. The fakes stay in the feature
  folders, never in a package.
- **Harness kinds:**
  - `apply,apply` is idempotent;
  - `apply,remove` leaves a clean ledger and a clean fake root, apart from the declared keeps (keys, config, logs);
  - an unknown authd build gives degraded;
  - a live installer-managed runsvdir gives 11;
  - a build mismatch gives 11 (Direct Summon);
  - the name staying taken skips Summon and hands back (Direct Summon).

### Gateway repo changes (small, separate)

- **`deploy/install-car.sh`.** Refuse both modes while `/run/direct-link-bin/BUILD` or `/run/direct-link-sv` exists, with "CoilShift
  packages are installed". That ends two per-boot paths rewriting each other.
- **`docs/SUMMON.md`.** Add "Installing through CoilShift", with the migration, checks and removal. Keep the installer section as
  the lab and fallback path.
- **The camera session's files.** `deploy/signin_precheck.sh` stays the way a new authd build is proven. Its result becomes a new row
  in `lib/gateway.sh`'s pin table and a new package version. Agree this with the camera session, which owns those files.

## Safety and security

- **Motion is unchanged.** The binaries are the same, and the run and finish scripts are the same apart from paths: D2, D3, H1, H4,
  the takeover and the deadman are untouched.
- **Our own crashes are handled as today.** If summonhelper's `serve` crashes, its `run` aborts and disconnects. If `run` crashes,
  runsv runs its `finish`.
- **CoilShift-initiated stops** happen on the owner's tap (an update or a removal, while parked) and go through the wrapper's TERM
  path, so the finish scripts run. A SIGKILL of the whole cgroup would skip them. That can only come from CoilShift or Lupine, never
  from our code: it is the residual risk the owner accepted with the Author Agreement.
- **The egress lock** is the same chain, re-asserted and checked at every boot. agw refuses to start without it.
- **Nothing in the public package needs to be secret.** Security rests on the car's own keys (authd's owner and driver roles) and the end-to-end
  sessions, not on hiding the binaries.
- **R22 holds.** No hook talks to 192.168.90.0/24.
- **The new behaviour is the unattended takeover at every boot.** Tesla's own WebRTC features (Tesla-app Summon, live camera) stay
  unavailable while Direct Summon is installed: the same as `SUMMON=1` today, but now without a typed yes.

## Questions to settle on the car before the first real install

Most are settled with a throwaway test package served from the same repository: a service that logs the signals and cgroup it sees,
and an apply hook that records its order and environment.
- **Q1.** Does apply run at every boot, and before services? The design does not depend on the order, because the wrapper waits for
  the marker, but the boot time does.
- **Q2.** Does a bind of an image mount stay executable?
- **Q3.** Can uids 4950 and 1337 traverse `/run/direct-link-bin` and exec from it? If not: an executable `PUT /v1/tmpfs` at that path,
  holding hash-checked copies.
- **Q4.** Is `$CSDK_RUN` writable and executable for root's runsv scripts?
- **Q5.** What does stopping a service send (TERM?), and how long is the grace? The wrapper needs about 20 s worst case; summonhelper's
  finish itself takes about 1-2 s and goes first.
- **Q6.** Does `rerun: in-place` rerun apply on an update without a reboot?
- **Q7.** What do exit codes 11 and 12 do to services, and to the status?
- **Q8.** Does `iptables` on 2026.38.6 accept the owner match? Today's installer already proves this on this car, so it is a check, not
  a risk.
- **Q9.** When does CoilShift stop or restart a package's services, apart from the owner's update and remove taps: after a plan
  change, a daemon restart, a run-mode switch, sleep? If routine stops are SIGKILLs of the whole cgroup, the finish scripts don't run,
  and the residual-risk argument above has to be revisited before Summon is trusted under CoilShift.

## Before the first publish (owner)

- **How the source reaches Lupine.** The Author Agreement wants the complete source of every version, by Lupine's method, before
  that version reaches any car, with no confidentiality (§1.4, §3.2, §3.4). For this package that includes the gateway's Go source
  and the scripts that build the payload images. The private repos name the car (VIN, tailnet address), so they can't simply be
  shared.
  - **Proposal:** a cleaned bundle for each version: a `git archive` of the gateway commit, the `direct-link/` build scripts and the
    toolchain pins. Run the same leak scan as `publish.sh`, then deliver it by Lupine's method.
  - **Open:** how source for the existing packages has reached Lupine so far, and whether that is the method Lupine wants.
- **Confirm the two publishing decisions:** the binaries become public, and Lupine gets the gateway source.

## Left to the builder

- The exact script structure and function names.
- The log wording, except the status reasons above.
- The harness seed details.
- README contents beyond the migration and the checks.
- The build-script interface.
- The order of the payload leak scan.
