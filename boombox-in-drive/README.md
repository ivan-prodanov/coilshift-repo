# boombox-in-drive

Boombox / Megaphone buttons usable **while driving**. **Verified live on-car
2026-11-08** (buttons enable in Drive).

2026.8.6-amd needs **TWO gates, in different functions**. Gate 1 alone is **not
enough** — that is exactly why coilshift's stock `enable-boombox` and the old
2026.32.7 script still disable the buttons once you shift into Drive.

Build: libQtCarGUI **2026.8.6-amd**. Offsets are per-build.

## Gate 1 — construction-time

`ExternalSpeakerSettings::makeBoomboxButtons` (0x2723b70) calls
`VehicleUtils::inDrivingGear` (0x3aaf790) at **`0x2723dcb`**; the result in `%r14d`
disables both buttons.

- Patch `e8c0b93801` (`call`) → `31c0909090` (`xor eax,eax; nop×3`).
- This is the **only** site coilshift `enable-boombox` / the old script touch — and on its own it does **not** hold in Drive.

## Gate 2 — runtime (the real gate)

`ExternalSpeakerSettings::updateButtonStates` (**0x2724b20** — *not*
`updateBoomboxButtonState` 0x2724820, which only sets labels) re-computes
enable/disable on every state change from `VehicleUtils::isCarParked`
(0x3aaf880, @plt 0x14736e0), called **4×**:

```
0x2724bb2   0x2724c3b   0x2724c51   0x2724d65
```

Shifting to Drive fires it → `isCarParked` false → re-disables, overriding gate 1.

- Patch each `call` (`e8…`) → `b801000000` (`mov $1,%eax` = force "parked").
- Stock bytes read back live: `e829ebd4fe`, `e8a0ead4fe`, `e88aead4fe`, `e876e9d4fe`.

## Scope — do NOT over-patch

- Do **not** patch global `isCarParked` (0x3aaf880) — used UI-wide.
- Leave `PhoneManager::inCall` (Megaphone mic-conflict gate) and
  `VehicleUtils::boomboxAllowed` (0x3abcb80, hw/country gate).
- Tell: `inDrivingGear` has **zero** callers once gate 1 is patched (makeBoomboxButtons
  was its only caller) — that confirms it is not the runtime gate.

## coilshift

`coilshift/io.github.ivan-prodanov.boombox-in-drive/rules/boombox_in_drive.json`. Stock coilshift
`enable-boombox` = `find bf01000000e8????????4189c684c0 span[5,10) → 31c0909090` plus
`hasPedestrianWarning → b801000000c3` stubs — it **misses the isCarParked gate**
(hence the Drive bug). This package adds the 4 `updateButtonStates` isCarParked calls
as symbol-scoped `find`/`span`/`replace → b801000000`, `live:"hot"`; patterns verified
unique + dry-run OK; `csdk rules test` on 2026.8.6 (2026-11-20) gives the same 4 offsets
and stock bytes as rows 2-5 of `boombox-in-drive.sh`. Gate 1 stays with the built-in
`enable-boombox`, which must stay installed. Built and checked by `tools/build.sh` (see the top-level README); the change-list digest for 2026.8.6 is in `rules-digests.tsv`.

## Files

- `boombox-in-drive.sh` — 5-row live `/proc/mem` patcher (reverts on UI restart; re-run `apply`)
- `coilshift/io.github.ivan-prodanov.boombox-in-drive/` — package source (`coilshift.json` + `rules/boombox_in_drive.json`); `tools/build.sh` builds it into `dist/public/`
