---
name: tesla-fw-patches
description: >-
  How this repo patches the Tesla MCU UI firmware (libQtCarGUI) on a live car
  over SSH: the two patch forms (live /proc/mem .sh patchers vs durable coilshift
  packages), how the car is reached over Tailscale, how to check which patches
  are currently applied, and the safety rules. Use this skill whenever the work
  touches this project at all — the car's SSH connection, Tailscale/.ts.net
  access, libQtCarGUI byte patches, coilshift packages, DataValues, or any of the
  features here (fart-in-drive, boombox-in-drive, horn-in-drive, reverse-locks,
  grok). Also use it before running ANYTHING against the car, even a read, so you
  apply the right connection string and the read-only-first rule.
---

# Tesla firmware patches (libQtCarGUI) — working on the car

This project hand-derives byte patches for the Tesla MCU UI library
`/usr/tesla/UI/lib/libQtCarGUI.so.1.0.0` and packages related tweaks, then
applies/checks them on a **real, specific car over SSH**. Read this before
touching the car.

## The car and how we reach it (Tailscale)

- **Target vehicle:** the owner's car — a Tesla running firmware
  `develop-2026.8.6`, UI lib build **`2026.8.6-amd`**. EU-homed, Premium
  Connectivity active. Its VIN, SSH host and tailnet live only in `local.env`
  (gitignored; see `local.env.example`), never in this repo, which is public.
  The MCU ("infoz") is a Buildroot Linux x86-64 box.
- **Connection:**

  ```bash
  . ./local.env   # CAR_SSH="-p 13400 root@<car host>"
  ssh $CAR_SSH
  ```

- **Tailscale is how the SSH reaches the car.** The car's `*.ts.net`
  hostname is a **Tailscale** address. Tailscale is a WireGuard-based mesh VPN:
  every enrolled device ("node") gets a stable address on a private "tailnet" and
  can talk to every other node directly, wherever it physically is. The car is on
  our tailnet, so `ssh …ts.net` connects straight to it with no port-forwarding or
  public IP. **This is an internal access mechanism, not a Tesla UI feature.** On
  the car itself, Tailscale is stood up by the on-car coilshift packages
  `dev.coilshift.tailscale` / `dev.coilshift.tailscale-native` (and surfaced by
  `dev.coilshift.ts-statusbar`); if SSH over the `.ts.net` name ever fails, the
  tailnet/those packages are the first thing to check, not the patch scripts.
- **The car gates SSH itself:** its `sshd` banner says *"REMOTE SSH NOT ALLOWED:
  customer vehicle / SSH allowed: vehicle parked."* So SSH only works while the
  car is **parked**. Don't expect to reach it in motion.

## Two forms of every feature — and what "apply" means

Each feature folder ships the same feature in up to two forms. **This matters
because only one of them is something we can actually install from this repo
right now.**

1. **Live `.sh` patcher** (e.g. `fart-in-drive/fart-in-drive.sh`) — edits the
   `/proc/<QtCar pid>/mem` of the running UI process. No rootfs write.
   - Interface is uniform: `apply | status | restore`.
   - **`status` is read-only** (reads bytes, compares to stock/patched — writes
     nothing). `apply`/`restore` write to process memory.
   - **Everything a `.sh` applies REVERTS on a UI restart or reboot** — re-run
     `apply` after each restart. So "is it applied" for these is a *point-in-time*
     question; re-check with `status`.

2. **`coilshift/io.github.ivan-prodanov.<name>/` package** — the durable form that
   survives reboots (byte rules v2 on role `browser-ui`, or hooks/services for non-byte
   tweaks like netflix-cdm and lock-chime-random), built with the CoilShift SDK's `csdk`.
   - **Toolchain:** `tools/get-csdk.sh` installs the pinned csdk 0.1.2 into `tools/bin/`
     (from the private repo `mLupine/coilshift-zero-sdk`; don't fork it). `tools/build.sh`
     lints, harness-tests and rule-tests every package against the 2026.8.6 and
     2026.38.6 libs in `../tesla-dump`, and checks the digests in `rules-digests.tsv`. Run it after any
     package or rule edit. A changed digest means bytes moved; `--record` is only for
     deliberate changes.
   - **IDs:** `dev.coilshift.*` is reserved (the car refuses it from added repos). The
     on-car built-ins keep that prefix (`enable-boombox`, `theater-webapps`, `grok-region`,
     `set-dvs`, ...). Our packages are `io.github.ivan-prodanov.*`.
   - **Getting one onto the car:** no signing. `tools/build.sh --site` → `tools/publish.sh`
     (pushes `site/` to `ivan-prodanov/coilshift-repo`, served at
     `https://ivan-prodanov.github.io/coilshift-repo/`) → add the URL in CoilShift → install. This is an outward-facing
     publish, and it requires accepting Lupine's Package Author Agreement. **Never publish
     without the user's explicit go-ahead.** `netflix-cdm` is public by the user's decision
     (2026-11-20, CoilShift agreed); its payload is Google's Widevine CDM. It's ONLY FOR OLD
     FIRMWARE WHERE THE BROWSER IS TOO OLD, and it exits 10 (unsupported) where the browser's
     CDM is already >= 4.10.3050.0. Keep that wording and that guard.
   - **Not published yet (2026-11-20):** nothing from this repo is installed as a package
     on the car. The durable features already there (`dev.coilshift.grok-region`,
     `dev.coilshift.enable-boombox`) are built-ins.
   - **Grok is out of scope:** the repo has no grok package any more (removed 2026-11-20),
     because CoilShift's built-in `dev.coilshift.grok-region` covers it. Don't recreate it.

> **In short:** from this repo, to change the car *now*, use the `.sh`
> `apply`/`restore`. The `coilshift/` packages pass csdk lint/test/rules but reach the
> car only once the user publishes a repository.

## Checking which patches are applied (read-only, do this first)

`scp`/writing files to the car is often blocked and unnecessary for a check.
To read the **live** byte-patch state without copying anything, pipe a read-only
status probe over SSH stdin. It finds the process that maps `libQtCarGUI`,
resolves its load base, and compares the bytes at each gate to the stock/patched
values. See [`scripts/check-live-patches.sh`](scripts/check-live-patches.sh) —
run it from the repo root:

```bash
.claude/skills/tesla-fw-patches/scripts/check-live-patches.sh
```

It prints, per gate, one of `stock` / `PATCHED` / `UNKNOWN(<bytes>)`. `UNKNOWN`
means the bytes match neither — usually a different build (offsets are
per-build), so stop and re-derive rather than writing. The probe's table is
2026.8.6-only; on 2026.38.x use `fart-in-drive.sh status` (it detects the build).

To check the **durable** state (coilshift packages, grok markers), read-only:

```bash
# installed coilshift packages
ssh $CAR_SSH \
  'ls /var/lib/coilshift/pkgs/ | grep -v "\.csp$"'

# grok / car-assist region state
ssh $CAR_SSH 'bash -s' <<'EOF'
echo "country: $(cat /var/etc/country 2>/dev/null)"
ls -la /tmp/car-assist/force-eu /tmp/car-assist/force-na 2>&1
for p in $(pgrep car-assist 2>/dev/null); do
  tr "\0" " " < /proc/$p/cmdline | grep -o "assistant-api[^ ]*"; done
EOF
```

## The feature inventory

Deep per-feature mechanics (exact gates, offsets, byte swaps) live in each
folder's `README.md` — read those, don't duplicate them here.

| Folder | What it does | Form(s) | Repo status |
|---|---|---|---|
| `fart-in-drive/` | Emissions fart button usable in gear + external (PES) fart in motion | `.sh` (3 gates) + coilshift | ✅ verified live 2026-11-08; 2026.38.3.2 and 2026.38.6 offline-verified only |
| `boombox-in-drive/` | Boombox/Megaphone buttons usable while driving | `.sh` (5 gates) + coilshift | ✅ verified live 2026-11-08 |
| `horn-in-drive/` | Boombox Horn Sound + Replace Horn usable in Drive; custom horn plays in gear; Replace Horn shown outside US/CA/MX (gate 6: `canSuppressHorn` = `VAPI_countryCode` in US/CA/MX; this car is DE) | `.sh` (6 gates) + coilshift | ✅ gates 1-5 verified live 2026-11-10; gate 6 offline only (2026-11-21) |
| `reverse-locks/` | Apps/entertainment usable in Reverse | `.sh` + coilshift (no built tgz) | 🛑 **DO NOT APPLY — dangerous** (breaks Drive/Park gear awareness, no UI gear control, needs a car reset to recover). `status`/`restore` only. |
| `grok/` | Airgapped Grok on the EU home shard (force-eu endpoint + hold `MEDIA_needAuthToken=false`) | `grok-eu.sh` only; the durable form is CoilShift's built-in `dev.coilshift.grok-region` | ✅ verified live 2026-11-10; out of scope now |

## Current applied-state snapshot (checked 2026-11-11)

Live `.sh` patches revert on UI restart, so re-run the status probe to refresh —
this is a point-in-time reading.

- **Packages from our repo (checked 2026-11-20, read-only):** `io.github.ivan-prodanov.netflix-cdm`
  1.0.0 installed and active (4 boot-scope ledger effects; CDM 4.10.3050.0 at `/run/netflix-cdm`;
  sandbox vars + tile UA overlays in place). `io.github.ivan-prodanov.lock-chime-random` 1.0.0
  installed: service + inotifywait running, backup in
  `/var/lib/coilshift/state/<id>/LockChime.orig.wav`, clips from `/mnt/media/usb-sdb3/LockChime`
  (12 wav). 1.1.0, with the in-app instructions, is published but the car hadn't refreshed yet.
  Fart/Boombox/Horn packages were **not** installed.

- **grok** → **ACTIVE (durable).** Signed `dev.coilshift.grok-region` installed
  (hooks + `watch.sh` daemon), `/tmp/car-assist/force-eu` present, car-assist
  endpoint = `assistant-api.prd.euw1…` (home shard). Note `/var/etc/country` is
  still `US`; grok works anyway because the package points car-assist at euw1 and
  holds `MEDIA_needAuthToken=false`.
- **boombox-in-drive** → **PARTIAL.** Only gate 1 (`makeBoomboxButtons`
  driving-gate) reads PATCHED live — that comes from the durable
  `dev.coilshift.enable-boombox` package. The 4 runtime `updateButtonStates`
  park-gates (what the repo `.sh` adds) are **stock**, so per this repo's own
  notes the buttons still re-disable once you shift to Drive. The full live patch
  is **not** currently applied — run `boombox-in-drive.sh apply` to complete it.
- **fart-in-drive** → **not applied** (all 3 gates stock).
- **horn-in-drive** → **not applied** (all 5 gates stock).
- **reverse-locks** → **not applied** (all stock) — and must STAY that way.
  🛑 **Never apply reverse-locks:** on-car it broke the car's Drive/Park gear
  awareness and left no UI way to change gears (needed a full reset). The `.sh`
  `apply` path is guarded to refuse. `status`/`restore` only.

Also installed on-car but outside this repo: `drive-locks` (the Drive counterpart
that `reverse-locks` complements), `carplay`(+`-float`), `tailscale`(+`-native`),
`ts-statusbar`, `vnc`, `ssh-key`, `ssh-armor`, `fsd-fix`, `set-dvs`, `no-more-ota`,
`more-apps`, `theater-webapps`, `neural-tts`, `no-elon`(+variants), and more.

## Safety rules when working on the car

- **Read before you write.** Run `status` / the read-only probe first and confirm
  the current bytes before any `apply`/`restore`.
- **Offsets are per-build.** Everything here is for `2026.8.6-amd`. The packages' rule sets
  for fart, boombox and horn are also checked on 2026.38.6 (offline, 2026-11-21); of the `.sh`
  patchers only fart-in-drive.sh knows 2026.38.x. On any other build the scripts report `UNKNOWN(...)`
  or "build not recognised" and refuse — re-derive offsets by symbol name
  (`nm -D` / `objdump`), never infer. Verify on the live car. On 2026.38.x,
  objdump mislabels PLT calls (`<prev stub>@plt+0xa`; the real callee is the label at
  target+6), and file offset ≠ vaddr (+0x1000). See `fart-in-drive/README.md`.
- **`status` is safe; `apply`/`restore` modify a running car's UI.** They only
  touch process memory (revert on restart), but still confirm with the user
  before writing, and know what each gate does first.
- **Don't casually write the car's rootfs** or install/remove coilshift packages
  over SSH. Durable changes go through CoilShift from a published repository (see
  above), and publishing is the user's call, never yours.
- **🛑 Never apply `reverse-locks`.** It breaks the car's Drive/Park gear
  awareness and leaves no UI way to change gears — confirmed on-car, recovery
  needed a full reset. Its `apply` is guarded to refuse; keep it stock.
- **Park-only SSH:** the car refuses SSH unless parked; don't try to reach it in
  motion.
- **This is the user's own car.** Treat the SSH host, VIN, and tailnet as private:
  they stay in `local.env`; never commit them or send them to external services.
  `tools/leakscan.sh --git` checks the repo before every push.
