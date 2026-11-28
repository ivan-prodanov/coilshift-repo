# Tesla UI firmware patches (libQtCarGUI)

Hand-derived byte patches for the Tesla MCU UI library, build **2026.8.6-amd**
(`develop-2026.8.6`). Each subfolder is one feature: a live `/proc/mem` bash
patcher and a coilshift package (the durable form).

This repository is **public** and also serves the built packages: the sources live on `main`,
the CoilShift repository (`index.json` + `pool/`) on the `gh-pages` branch, which GitHub Pages
serves at **`https://ivan-prodanov.github.io/coilshift-repo/`**. Add that URL in CoilShift.

**Private settings stay out of git.** The car's VIN, SSH host and tailnet, the owner phone's
key and the leak patterns live in `local.env` (gitignored; copy
[`local.env.example`](local.env.example)), which the tools read. Run `tools/leakscan.sh --git`
before every push: it scans every file and every git object and refuses on any hit.

## Target

`/usr/tesla/UI/lib/libQtCarGUI.so.1.0.0` (x86-64, ~80 MB). The lib is
`.symtab`-**stripped but keeps a full `.dynsym`** (~138k symbols), so offsets are
resolved by symbol name — no Ghidra pass needed:

```bash
nm -D libQtCarGUI.so.1.0.0 | grep -i <name> | while read a t n; do echo "$a $t $(echo "$n"|c++filt)"; done
objdump -d --start-address=0xFUNC --stop-address=0xNEXT libQtCarGUI.so.1.0.0   # read the site
rasm2 -a x86 -b 64 -d <hex>                                                     # decode/encode bytes (radare2)
```

Stock `call` bytes = `e8` + rel32, where `rel32 = target − (site + 5)`.

> **All offsets are per-build.** Re-derive for any other firmware. **Verify on the
> live car; never infer** — `nm`/`objdump` can share one symbol label across two
> adjacent functions, so confirm the real gate empirically.

## Two forms per feature

- **`*.sh`** — patches `/proc/<pid>/mem` of the running QtCar process. No rootfs
  write. **Reverts on UI restart** (re-run its `apply`). Good for testing.
- **`coilshift/io.github.ivan-prodanov.<name>/`** — the durable CoilShift package
  (`coilshift.json` schema 1 + `rules/*.json` byte rules v2 and/or hooks), built with the
  CoilShift SDK's `csdk`. See [Building the durable packages](#building-the-durable-packages-csdk).

coilshift rule shape: `find` = hex where each `??` is one wildcard byte
(so `e8????????` = any `call`); `span:[a,b)` picks which bytes of the match to
overwrite; `replace` = hex. Sites are symbol-scoped so a pattern matches once.

## Building the durable packages (csdk)

The SDK is the private repo [`mLupine/coilshift-zero-sdk`](https://github.com/mLupine/coilshift-zero-sdk):
docs in its `docs/` (manifest, car API, byte rules v2), and the closed-source `csdk` /
`csdk-harness` binaries on its Releases page. It is a toolchain, not something to fork.

```bash
tools/get-csdk.sh     # pinned csdk + csdk-harness (0.1.2) into tools/bin/, checked against SHA256SUMS
tools/build.sh        # lint, harness-test, rule-test and build every package into dist/
```

`tools/build.sh` checks each package with `csdk lint --strict`, runs `csdk test` (`apply,apply`
must equal one apply; `apply,remove` must leave an empty ledger and an unchanged root),
runs `csdk rules test` against the `libQtCarGUI.so.1.0.0` of each firmware build in `../tesla-dump`
(2026.8.6, the car's and the primary build, and 2026.38.6; `FW_LIBS=` to override), and compares each
rule set's change-list digest with [`rules-digests.tsv`](rules-digests.tsv). A changed digest means a
locator edit moved bytes, so the build fails. Every rule set needs a row for the primary build. On
another build a rule set counts as supported only once its row is added by hand, after that build's
gates were checked; until then its result is shown and not enforced. Today fart, boombox and horn
are checked on 2026.38.6, and voice-button-swap has its own blob for each of the two builds. After a deliberate change, run `tools/build.sh --record` and review the diff.
Every package is public today (`dist/public/`, then `site/`). Payloads, which today means
netflix-cdm's CDM image, are staged in `dist/public/payloads/<sha256>` and land in
`site/pool/`. Grok has no package here; CoilShift's built-in `dev.coilshift.grok-region`
covers it.

**IDs.** Every package is `io.github.ivan-prodanov.<name>`. `dev.coilshift.*` is reserved:
the car refuses it from any repository you add, and `csdk` refuses to package it.

**Getting a package onto the car.** Outside packages are not signed. The car takes them
from a repository listed in CoilShift, and the CoilShift server fetches that repository:

1. `tools/build.sh --site` writes `site/` (`index.json` + `pool/<sha256>`) from `dist/public/`.
2. `tools/publish.sh` pushes exactly `site/` to this repo's `gh-pages` branch, which
   GitHub Pages serves at **`https://ivan-prodanov.github.io/coilshift-repo/`** (live since
   2026-11-20; served from `gh-pages` since the sources moved here). It refuses to push if
   `tools/leakscan.sh` finds the car's identifiers (local.env's `LEAK_PATTERN`), a tailnet
   address or a local path in `site/`. Anyone with the URL can download every package. Bump a
   package's `version` whenever its archive changes, or cars that already have that version
   won't see the update.
3. Add that URL on CoilShift's Repositories page and install from there. The daemon then
   re-applies the package on every boot, so `apply-all.sh` isn't needed for it. CoilShift
   re-reads the repository about every 6 hours, or straight away when you tap Refresh.

> **Before step 3:** serving a repository to enrolled cars means accepting the
> CoilShift Package Author Agreement (`AUTHOR-AGREEMENT.md` in the SDK). You hand Lupine the
> full source of every version, grant a perpetual, sublicensable licence to it (they may ship it
> closed-source as part of CoilShift), and accept terms Lupine can change without notice.
> Decide that before publishing anything.
>
> **`netflix-cdm` is ONLY FOR OLD FIRMWARE WHERE THE BROWSER IS TOO OLD** (Netflix error E109,
> e.g. 2026.8.6). It skips itself, as *unsupported*, on firmware whose browser already has
> Widevine 4.10.3050.0 or newer. Its payload is Google's Widevine CDM, published by the
> owner's decision on 2026-11-20 after CoilShift agreed. Google's licence for that file is
> a separate matter from the CoilShift agreement.
>
> **Built-ins.** Boombox in Drive needs the built-in `dev.coilshift.enable-boombox`, and
> netflix-cdm needs the built-in `dev.coilshift.theater-webapps`; keep both installed.

## Re-applying after a reboot — [`apply-all.sh`](apply-all.sh)

The MCU **suspends to RAM** when the car sleeps (`/sys/power/state` = `freeze standby mem`);
it does **not** reboot — `/run`, the running UI, and every patch survive a normal
park/sleep (uptime just keeps counting; e.g. it was up ~36 h straight through a full day of
work). Patches only clear on:

- a **QtCar UI restart** → clears the `/proc/<QtCar>/mem` byte patches (fart/boombox/horn);
- a **true MCU reboot** (OTA, crash/watchdog reset, scroll-wheel reset, 12 V loss) → clears
  the `/run` binds (netflix CDM) **and** restarts the UI, so it clears everything.

`apply-all.sh` re-applies **all** enabled features in one idempotent pass (every feature
script self-checks: "already patched" / readback-verified). Run it on the car as root
after an OTA or a UI crash. It is **not** a boot hook — the rootfs `/etc/sv` is read-only,
so no writable boot seam is wired; trigger it yourself (or hook it into whatever boot
mechanism you add). CoilShift's own mods re-apply at boot via the **`ssp`** daemon
(`ssp-watchdog` + one `ssp-client` per enabled mod); a feature run from its `.sh` doesn't get
that, which is exactly what `apply-all.sh` stands in for. A feature installed as a package
from a repository does get it, so it doesn't need `apply-all.sh`. Its header carries the one-time
staging (tar-over-ssh, since the car's sshd has no sftp) and the editable feature list.

## Features

| Folder | What | Status |
|---|---|---|
| [`fart-in-drive/`](fart-in-drive/) | Emissions fart button usable in gear, + external (PES) fart in motion | ✅ **verified live** 2026-11-08 (3 gates); 2026.38.3.2 and 2026.38.6 offline-verified |
| [`boombox-in-drive/`](boombox-in-drive/) | Boombox / Megaphone buttons usable while driving | ✅ **verified live** 2026-11-08 (2 gates) |
| [`reverse-locks/`](reverse-locks/) | Apps / entertainment usable in Reverse | ⚠️ **partial, abandoned** — apps launch, backup camera stays fullscreen |
| [`grok/`](grok/) | Airgapped Grok on the home-region assistant shard (force-eu endpoint + hold `MEDIA_needAuthToken=false`) — *assistant config + DataValue hold, not a libQtCarGUI byte patch* | ✅ **verified live** 2026-11-10 |
| [`horn-in-drive/`](horn-in-drive/) | Boombox Horn Sound + Replace Horn freely usable in Drive; custom horn (OldHorn) sounds when honking in gear; Replace Horn shown outside US/CA/MX | ✅ **verified live** 2026-11-10 (gates 1-5); gate 6 (Replace Horn visible) offline-verified 2026-11-21 |
| [`netflix-cdm/`](netflix-cdm/) | Netflix tile actually plays on 8.6 — newer Widevine CDM (4.10.3050) bound into the Netflix webapp sandbox + clean UA + freed profile disk, keeping the stock Chrome/140 engine — *Widevine CDM swap + sandbox bind + webapp-config edit, not a libQtCarGUI byte patch* | ✅ **verified live** 2026-11-12 |
| [`lock-chime-random/`](lock-chime-random/) | Lock chime plays a **random** `*.wav` from a USB `LockChime` folder instead of the single `LockChime.wav` — *`QtCarAudiod` re-reads the file (in-process ALSA) on every lock, so a userspace restage-on-close daemon swaps in a fresh random clip between locks; not a byte patch. Durable form = a CoilShift `service`* | ✅ **verified live** 2026-11-12 |
| [`close-on-lock/`](close-on-lock/) | On every lock (not in R/N/D): an open trunk closes (`GUI_rearTrunkRequest=Close`, close-only), and an open frunk gets the open command again, which closes an aftermarket power frunk — *a polling CoilShift `service`, the touchscreen's own requests; not a byte patch* | 📦 **published** 2026-11-28; 1.0.0 was refused by the car's CoilShift (no apply hook), 1.0.1 adds one; not yet tried on the car |
| [`voice-button-swap/`](voice-button-swap/) | Steering-wheel voice button: short press starts Grok, long press the Tesla assistant, only when no assistant is open (closing/muting Grok, confirming commands and dictation stay stock); hides the "Long press for Grok" hint. 2026.38.6 and 2026.8.6; *a code blob over the hint function + the two slot jumps of `VoiceControlHandler::qt_static_metacall`* | 🧪 **offline-verified** 2026-11-26 on 2026.38.6 and 2026.8.6; not yet tried on the car |
| [`direct-link/`](direct-link/) | The phone app's car side (gateway, camera, talk, dashcam) as a CoilShift package that comes back at every boot, instead of `install-car.sh --after-boot` over SSH — *today's reviewed binaries and runit scripts under CoilShift* | 📦 **published** 2026-11-27 (1.0.0, gateway eccb2c4); 1.2.4: direct mode + Direct commands, Summon's deadman fitted to LTE, gates 6-9 dropped (gateway 3a288f7) |
| [`direct-summon/`](direct-summon/) | Summon from the phone on top of Direct Link (summonhelper takes `com.tesla.Webrtc` from stock) — *same binaries and scripts as today's `SUMMON=1`* | 📦 **published** 2026-11-27 (1.0.0, gateway eccb2c4); 1.0.8 (gateway 3a288f7) |
| [`gateway/`](gateway/) | Go source of the car-side binaries in Direct Link and Direct Summon (agw, camhelper, summonhelper), at the gateway commit the packages are built from — *source only, built by `direct-link/build-payloads.sh`; docs and tests scrubbed of the owner's identifiers (the tests use a made-up phone key), which leaves the binaries byte-identical* | 📦 `3a288f7` (direct mode + Direct commands; D2 at 500/800 ms; gates 6-9 dropped) |

See each folder's `README.md` for the exact gates, offsets, and byte swaps.
