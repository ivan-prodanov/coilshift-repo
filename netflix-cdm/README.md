# netflix-cdm

> ⚠️ **ONLY FOR OLD FIRMWARE WHERE THE BROWSER IS TOO OLD.** Use it only when Netflix says
> "your browser is too old" (E109): firmware whose browser ships a Widevine CDM older than
> 4.10.3050.0, such as 2026.8.6 (4.10.2934.0). Newer firmware, such as 2026.38.3.2 and 2026.38.6, already
> ships 4.10.3050.0; there the package changes nothing and reports itself *unsupported*.

**Netflix plays on 2026.8.6.** Verified live on-car 2026-11-12 (the owner's car,
fw `develop-2026.8.6`, CoilShift `theater-webapps` Netflix tile).

> Not a `libQtCarGUI` byte patch like most features here — this one is a **Widevine CDM
> swap + a sandbox bind + a webapp-config edit** (no lib patching). Mechanism differs;
> folder layout is the same.

## Symptom

The CoilShift Netflix tile (`dev-coilshift-theater-webapps.netflix`) opened to **"your
browser is too old"** — Netflix error **E109** (and M7703-1003 / M7399-1260 in other
states). Preview trailers *played with sound*, but no full title would start.

## What it is NOT (dead ends, so you don't repeat them)

- **Not a browser-version/UA gate.** The stock browser is Chrome **140.0.7339.207**
  (`/usr/lib/tesla-chromium/tesla-chromium-main`). We fed Netflix a clean, honored
  `Chrome/148` user-agent (Tesla's CEF *does* honor `--user-agent`; confirmed by reading
  the live `/json/version` over a temporary `--remote-debugging-port` inside the
  `chromium-entertainment` netns) — and Netflix **still** threw E109. So the UA string is
  not the gate.
- **Not fixable by transplanting the 148 engine.** The native `tesla-chromium` 148 from
  the 2026.38.3.2 OTA *runs* on 8.6 (we bound it in via the sandbox vars — see below), but
  it has **no audio at all** (its CEF audio pipeline doesn't emit on 8.6's older base) and
  is **unstable** (renderer crash, touch broke). Needs newer base libs / a wider
  `tesla-chromium.kafel` seccomp set than 8.6 ships. Abandoned — **keep the stock 140
  engine.**

## Root cause (what actually gates it)

**A Widevine CDM-version gate.** The stock browser bundles Widevine CDM **4.10.2934.0**
(`/usr/lib/tesla-chromium/WidevineCdm/manifest.json`); Netflix's player checks the real
CDM via EME and rejects it as too old. The 2026.38.3.2 OTA ships **4.10.3050.0** — same
4.10 line, so it loads cleanly on the 140 host. Swapping just the CDM flips Netflix from
E109 → playing.

Two more things were required for playback:

- **A full webapp profile disk.** `/opt/games/var` was **100% full** (stale duplicate
  `WebApp.theater-apps.*` caches), so Netflix's IndexedDB failed with
  `FILE_ERROR_NO_SPACE`. Freeing it (~300 MB) is part of the fix.
- **A clean UA.** The tile shipped `CHROMIUM_CONFIG_USERAGENT_PREFIX="TESLA_"`, producing
  a UA that doesn't start with `Mozilla/5.0` and trips detection. We drop the prefix and
  set a clean `Chrome/148` UA. (The CDM is the hard gate; this is belt-and-suspenders and
  matches the exact verified-working combination.)

## Fix (three levers, all reversible, rootfs untouched)

1. **CDM swap** — loop-mount a squashfs of the 148 `WidevineCdm` dir and **bind it into
   the Netflix sandbox**. The catch: every chromium surface runs under
   `minijail0 -v -P` with a **non-recursive `-b/usr,/usr`**, so a plain bind over
   `/usr/lib/tesla-chromium/WidevineCdm` (a submount of `/usr`) is **not** carried into the
   sandbox. You must add an explicit bind to the surface's minijail. Netflix uses
   `/etc/sandbox.d/vars/chromium-entertainment.vars` (selected by the tile's
   `CHROMIUM_CONFIG_SANDBOX_PROFILE=chromium-entertainment`); that file is on the read-only
   rootfs, so we patch a copy and `mount --bind` it over the original, inserting:
   ```
   -b/run/netflix-cdm,/usr/lib/tesla-chromium/WidevineCdm
   ```
   The bind only lands on a **fresh** webapp launch, so kill pre-warmed Netflix procs after.
2. **Clean UA** — in the tile config (writable): clear the `TESLA_` prefix, append
   `--user-agent=…Chrome/148…`.
3. **Free disk** — if `/opt/games/var` is low, delete the stale `WebApp.theater-apps.*`
   caches.

Verify: a fresh Netflix sandbox shows CDM **4.10.3050.0**
(`grep version /proc/<pid>/root/usr/lib/tesla-chromium/WidevineCdm/manifest.json`).

## Two forms

- **`netflix-cdm.sh`** — standalone on-car re-apply script (run over SSH after a reboot).
  Needs the CDM image staged once at `/opt/games/usr/local/netflix-fix/widevine-3050.sqfs`
  (header has the one-time upload; the file persists on `/opt/games/usr`, only the binds
  need re-applying each boot). Prints a verify line.
- **`coilshift/io.github.ivan-prodanov.netflix-cdm/`** — the durable package (`coilshift.json`
  schema 1 + `hooks/{apply,remove}.sh` + vendored `lib/csdk.sh`; the CDM is a manifest
  **payload**, fetched by hash and kept out of the archive, source file in `payloads/`).
  Everything goes through the car API, so the daemon's ledger undoes it: the CDM image
  (`PUT /v1/images/widevine`), its bind at `/run/netflix-cdm` (`PUT /v1/binds`), and two
  overlays (`PUT /v1/overlays`), one for the Netflix tile config with the clean UA and one
  for `chromium-entertainment.vars` with the CDM bind. Nothing is edited in place any more,
  not even theater-webapps' tile. The overlay contents are byte-identical to what
  `netflix-cdm.sh` writes, checked against the 2026.8.6 dump in the SDK harness.
  `apply` is on the **late / after_restart** phase, so CoilShift re-runs it every boot.
  Passes `csdk lint --strict` and `csdk test`.

> **Old firmware only, enforced.** Before touching anything, `apply` reads the browser's own
> `/usr/lib/tesla-chromium/WidevineCdm/manifest.json`. If that's already 4.10.3050.0 or
> newer, or unreadable, it removes anything an earlier run left, sets the status to
> *unsupported* with the reason, and exits 10. `tools/build.sh` checks both cases on seeded
> roots built from the dumps (`harness-seed.sh`): a 2026.8.6 root must apply and remove
> cleanly, and a 2026.38.6 root must exit 10 and stay byte-identical.
>
> **Published.** The package and its CDM payload are in the public build (`site/pool/`) since
> 2026-11-20: the owner's decision, after CoilShift agreed. The payload is Google's Widevine
> CDM, so Google's licence for that file is a separate question from the CoilShift agreement.
> Don't run this package and `netflix-cdm.sh` together: they bind the same paths.

> **Reboots:** the CDM loop-mount and the vars bind live in `/run` + the root mount
> namespace and don't survive a power cycle (the staged `widevine-3050.sqfs` on
> `/opt/games/usr` does). The **standalone `netflix-cdm.sh`** needs a manual re-run after
> a reboot (`apply-all.sh` does it).

## Payload

`payloads/widevine-3050.sqfs` (manifest path `payload/widevine-3050.sqfs`, sha256
`6a528d5deb7a01c7388f9d24aff6c246daf5d161fc415fa23e64a456c57f48ef`) — gzip squashfs
(~16 MB) of the Widevine CDM **4.10.3050.0** directory (`libwidevinecdm.so` +
`manifest.json`), lifted from the 2026.38.3.2 OTA's
`/usr/lib/tesla-chromium/WidevineCdm`. Rebuild from an extracted OTA with:
```
mksquashfs <ota>/usr/lib/tesla-chromium/WidevineCdm widevine-3050.sqfs -comp gzip -all-root -noappend
```

## Files

- `netflix-cdm.sh` — standalone on-car re-apply script (+ one-time upload instructions).
- `coilshift/io.github.ivan-prodanov.netflix-cdm/` — package source (`coilshift.json` +
  `hooks/{apply,remove}.sh` + `lib/csdk.sh`). Public; old firmware only (see above).
- `harness-seed.sh` — builds the old/new firmware fake roots `tools/build.sh` tests on.
- `payloads/widevine-3050.sqfs` — the CDM image (the package's payload, and the file
  `netflix-cdm.sh` expects staged on the car).

> A Mac-side orchestration helper that builds + uploads + applies in one go lives in the
> sibling analysis repo at `tesla-dump/compare/ports/netflix-on-8.6.sh`
> (`all|stage|apply|verify|revert`).
