# Direct Link and Direct Summon Implementation Plan

> **For agentic workers:** execute task by task (superpowers:executing-plans). Steps use checkboxes.
> Owner rule: implement + test + at most one review, then ship. Deep review only for the Summon takeover and stop paths.

**Goal:** Package today's working car side (gateway `d91f0e1`) as the CoilShift packages Direct Link and Direct Summon, so it
comes back by itself at every boot.

**Architecture:** No new behaviour. Each package mounts a squashfs image of today's binaries, writes today's runit run, finish
and log scripts (paths changed only), and runs them under its own `runsvdir` inside one CoilShift service. The apply hooks do
what `install-car.sh --after-boot` does today, unattended.

**Tech Stack:** bash hooks with the vendored `lib/csdk.sh` (copy from `netflix-cdm`), runit, squashfs-tools 4.7.5, Go via
the gateway's `make car`.

**Spec:** `direct-link/DESIGN.md`. **Source of truth for every script:** `deploy/install-car.sh` at gateway `d91f0e1`
(`git -C ../airgapp/gateway show d91f0e1:deploy/install-car.sh`). Port it; don't redesign it.

## Global Constraints

- IDs `io.github.ivan-prodanov.direct-link` / `io.github.ivan-prodanov.direct-summon`, titles "Direct Link" / "Direct Summon",
  version 1.0.0, build id = short gateway commit.
- New paths: `/run/direct-link-bin`, `/run/direct-summon-bin`, `/run/direct-link-sv`, `/run/direct-summon-sv`,
  `/var/lib/airgapp-gw/direct-link.conf`. Every other path, uid (4950, 1337) and the chain `AIRGAPP_GW_OUT` stay as today.
- Generated run/finish/log scripts must equal the installer's output for the same inputs, apart from the path map above.
- Under `CSDK_FAKE_ROOT`, every `sv`, `iptables`/`ip6tables`, `kill`, `gdbus`, `runsvdir` call goes through a `do_*` wrapper
  that runs a fake. No test may reach a real tool or a real pid. Kill only by a pid read from a file, checked by `comm`.
- Nothing to 192.168.90.0/24; `check_ip` refuses that range for `PI_IP`.
- No VIN, keys, phone pins, tailnet name or `/Users` path in a package or payload.
- Never publish or push without the owner's go-ahead.

## Review Focus

- An update or removal while a Summon session is open: the stop path runs `sv down` on Summon first, so its finish aborts.
- A bad `direct-link.conf` (wrong IP or writer): apply fails with a clear status and writes no agw service.
- A leftover `applied` marker from an older boot: `supervise.sh` keeps waiting (boot id mismatch).
- Removing Direct Link removes Direct Summon first, and Summon's remove works without Direct Link's binds.
- A second `supervise.sh` while an old `runsvdir` still runs on the same svdir: it refuses to double-start.

---

### Task 1: Payload build

**Files:** Create `direct-link/build-payloads.sh`, `direct-link/payloads/`, `direct-summon/payloads/`.

- [ ] `build-payloads.sh <gateway-commit> <agw-sha> <camhelper-sha> <summonhelper-sha>`: `git archive` the commit from
  `$root/../airgapp/gateway` (no `/Users` default) into a temp dir, run `make car` unchanged, refuse on any hash mismatch.
- [ ] Pack `direct-link-<build>.sqfs` (agw, camhelper, BUILD) and `direct-summon-<build>.sqfs` (summonhelper, BUILD) with
  `mksquashfs <dir> <out> -comp gzip -all-root -noappend -no-xattrs -mkfs-time 0 -all-time 0`.
- [ ] Write each image's sha256 and size into its manifest's `payloads`, and the same `lib/gateway.sh` (binary hashes, build
  id, authd pin table: 2026.38.6 `e0c74323…`, 2026.8.6 `33d62d9e…`, full values from the camera runbook) into both packages.
- [ ] Leak scan: `unsquashfs` each image to a temp dir, run `tools/publish.sh`'s pattern plus `/usr/local/go`, `/private/tmp`,
  `/var/folders` over files and `strings`. Must be clean.
- [ ] Check: build at `d91f0e1` twice; the binaries match the reviewed hashes and both runs give identical image sha256.
- [ ] Commit.

### Task 2: Direct Link package

**Files:** Create `direct-link/coilshift/io.github.ivan-prodanov.direct-link/{coilshift.json, hooks/apply.sh,
hooks/remove.sh, bin/supervise.sh, lib/csdk.sh, lib/sys.sh, lib/templates.sh}`, `direct-link/harness-seed.sh`,
`direct-link/test/fakes/`, `direct-link/test/run.sh`.

- [ ] `lib/sys.sh`: `do_sv`, `do_iptables`, `do_ip6tables`, `do_kill`, `do_gdbus`, `do_runsvdir`, plus `check_ip`,
  `egress_chain_ok`, `egress_jump_first`, ported from the installer.
- [ ] `lib/templates.sh`: the installer's agw and camhelper run/finish/log templates with the path map applied.
  Test (`test/run.sh`): render installer and package templates for the same inputs; the diff is exactly the path map.
- [ ] `hooks/apply.sh`: design steps 1-7 (refuse a live installer `runsvdir` with 11, mount + hash check, first-run
  provisioning, egress lock, legacy check, scripts, marker + status + restart on update).
- [ ] `hooks/remove.sh`: design remove steps; keeps keys, allowlist, config, logs.
- [ ] `bin/supervise.sh`: wait for this boot's marker, refuse a second `runsvdir`, on TERM/INT/HUP `sv -w 15 down` each
  service then HUP `runsvdir` by pid, exit non-zero if `runsvdir` dies. Unit tests in `test/run.sh` with fakes for the
  marker wait, the stop order and the double-start refusal.
- [ ] `harness-seed.sh` kinds: `apply,apply` idempotent, `apply,remove` clean, unknown authd → degraded, live installer
  `runsvdir` → 11, bad `direct-link.conf` → failed.
- [ ] Check: `tools/build.sh` passes for this package; commit.

### Task 3: Direct Summon package

**Files:** Create `direct-summon/coilshift/io.github.ivan-prodanov.direct-summon/…` (same layout, `lib/sys.sh` and
`bin/supervise.sh` copied from Task 2), `direct-summon/harness-seed.sh`, `direct-summon/README.md`.

- [ ] Manifest: `requires.packages` and `hooks.apply.after` name Direct Link.
- [ ] `hooks/apply.sh`: build-id match (11 on mismatch), mount + hash check, provisioning, the S0b takeover exactly as the
  installer does it (60 s bounded name wait; hand stock back and report degraded if it stays taken), summonhelper scripts.
- [ ] `hooks/remove.sh`: `sv -w 15 down`, wait for the process to go, remove `live`/`.live.*`, `stock_hand_back`.
- [ ] `bin/supervise.sh` adds take-it-back: at start and every 10 s, at most once a minute, the same bounded takeover.
  Unit-test the rate limit and that it never runs while our summonhelper runs.
- [ ] Seed kinds: build mismatch → 11, name stays taken → skip + hand back, remove without Direct Link's binds works.
- [ ] Review: one fresh reviewer on the takeover, stop and remove paths. Commit.

### Task 4: Repo wiring and runbook

**Files:** Modify `tools/build.sh` (`allow_warnings`: `direct-link direct-summon`, plus the payload scan), root `README.md`
and `.claude/skills/tesla-fw-patches/SKILL.md` rows; create `direct-link/README.md` (migration, checks, provisioning).

- [ ] Full `tools/build.sh` passes; commit.

### Owner-gated (stop here and ask)

Scope (owner, 2026-11-26): only the two packages and what they need. The installer guard and the probe package are dropped;
the migration retires the installer by hand, and design Q1-Q9 are checked during the first install with the owner present.

1. Settle how the source reaches Lupine; publish both packages.
2. Migration on the car with the owner (README), the checks, Q1-Q9 as far as they show, then Summon checks with Summon open
   (no motion).

## Rulings during the build (2026-11-26)

- Log paths are today's exactly (`/var/lib/airgapp-camhelper/svlog` for camhelper), not the
  design table's `svlog/camhelper`: the scripts must equal install-car.sh's.
- No `apply.log` file: the harness requires a second apply to leave the fake root unchanged, so
  hooks log to CoilShift's own log (`csdk_log`) and status only.
- `hooks.apply.after` cannot name an outside package (SDK refuses it); `requires.packages`
  already orders Direct Summon's apply after Direct Link's.
- CoilShift binds are plain `mount --bind` (no read-only option): `/run/direct-*-sv` are rw.
- Summaries are at most 280 bytes (SDK limit).
- direct-link.conf is made at the first apply from today's installed scripts (keeps PI_IP and
  the writer of a migrated car), then defaults.
- `csdk_api` gets a time limit (30 s in hooks, 3 s in supervise.sh): a stalled daemon socket
  must never hold up the stop path.
- Review fixes (one review of the Summon paths): runsvdir is recognised by argv[0]'s basename
  (the car starts it by path); the stop path takes summonhelper down before anything else;
  take-back only acts when summonhelper is set up, runs as a background job, and hands stock
  back if the name stays taken; apply's give-up and an exit trap use install-car.sh's
  stock_hand_back order, but stock is restarted even when the name has an owner (with ours gone
  that is stock itself or a stranger, never a free name with stock stopped).
- tools/build.sh: seed outcome `exit-N` (a refusal: exits N, changes nothing) and opt-in
  `--image-tree`; tools/publish.sh also scans squashfs payloads.
- 2026-11-27, Direct Summon 1.0.1: 1.0.0's install was rejected by CoilShift's daemon
  ("invalid_plan: plan contains unrequested or duplicate selections"; Direct Link 1.0.0 was
  already installed). Likely cause: `requires.packages` naming Direct Link, which the server
  compiles into the plan. 1.0.1 drops it; its apply waits up to 2 min for Direct Link's BUILD,
  then refuses a missing or different build as before.
