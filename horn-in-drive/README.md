# horn-in-drive ✅

Make the Boombox **Horn Sound** + **Replace Horn** toggles freely checkable/uncheckable **while
driving** (behave exactly like Park), so the chosen horn sound plays when you honk in gear.

> **Status: ✅ VERIFIED LIVE on-car 2026-11-10 (gates 1-5); gate 6 (Replace Horn visible outside
> US/CA/MX) added 2026-11-21. Gates 1-6 read PATCHED in the live 2026.38.6 UI (2026-11-24). Gates 7-8
> (toggles no longer reset at UI start, behind the `keep_toggles` setting) were added 2026-11-24 in v1.2.0
> and are offline-verified only.** Both toggles are fully interactive in
> Drive and the chosen horn (`GUI_pesTheChosenHorn` = OldHorn) sounds when honking while driving.
> 2026.8.6-amd build; offsets are per-build. Live `/proc/mem` patch reverts on UI restart; the coilshift
> package is the durable form.

## The 6 gates (all verified against the binary)

| # | Function @ site | Gate | Patch |
|---|---|---|---|
| 1 | `ExternalSpeakerSettings::externalSpeakerStateChanged` @ `0x2725148` | `isCarParked` | `e893e5d4fe`→`b801000000` (routes to the enable branch) |
| 2 | `externalSpeakerStateChanged` @ `0x2725245` | `customPedestrianWarningSoundAllowed` | `e8d685d3fe`→`b801000000` (→ `enableSwitch(true)`) |
| 3 | `ExternalSoundDropdown::updateActiveState` @ `0x270b89a` | `inDrivingGear` | `e8811ed7fe`→`31c0909090` |
| 4 | `ExternalSoundDropdown::updateButtonAvailabilty` @ `0x270bacd` | `inDrivingGear` | `e84e1cd7fe`→`31c0909090` |
| 5 | `ExternalSpeakerManager::onShiftStateChanged` @ `0x2721b87` | reset branch | `7417`→`9090` |
| 6 | `ExternalSpeakerSettings` constructor @ `0x2727019` | `canSuppressHorn` (country) | `e8920dd6fe`→`b801000000` (Replace Horn switch is created) |
| 7 | `ExternalSpeakerManager::updateFactoryLowHornNoiseAreaMode` @ `0x2721d44` (38.6: `0x2f396d9`) | `setValue(GUI_disableHorn, false)` | call retargeted to `DataValueManager::instance()` (`e85745d5fe`→`e8f7c2d1fe`; 38.6 `e8d20f4dff`→`e8d2fb57ff`), only with setting `keep_toggles` on |
| 8 | same function @ `0x2721d58` (38.6: `0x2f396ed`) | `setValue(GUI_pesHornSoundEnabled, false)` | the same (`e84345d5fe`→`e8e3c2d1fe`; 38.6 `e8be0f4dff`→`e8befb57ff`) |

**Key insight:** the toggle grey-out lives at the **screen** level, not the dropdown functions. Gate 1
routes `externalSpeakerStateChanged` to its parked branch; **gate 2 is the one that actually keeps "Horn
Sound" interactive** — `customPedestrianWarningSoundAllowed` returns false in Drive and feeds
`enableSwitch(false)`, so forcing it true gives `enableSwitch(true)`. **Gates 1+2 are both required.**
Gate 5 stops Drive from clearing Replace Horn (`GUI_disableHorn`). Playback itself is not drive-gated
(only a factory GPS geofence), so once a toggle is on, the horn sounds.

## Gate 6: Replace Horn hidden outside US/CA/MX (added 2026-11-21, not yet tested on the car)

The **Replace Horn** switch isn't greyed out, it's never created. When the Boombox screen is built,
`ExternalSpeakerSettings::ExternalSpeakerSettings(DisplayView*)` calls
`VehicleUtils::canSuppressHorn()` right after `populateDropdowns()`, and only if that returns true does
it look up the `"Horn"` dropdown and call `ExternalSoundDropdown::createReplaceHornSwitch()`.
`canSuppressHorn()` checks one thing: `VAPI_countryCode` is `US`, `CA` or `MX` (`0x5553`, `0x4341`,
`0x4d58`). It has no other caller in libQtCarGUI. This car reads `VAPI_countryCode = "DE"` (via
`/usr/local/bin/lv`), so the switch never appears, with or without gates 1-5. `/var/etc/country` and
`GUI_navVehicleCountry` don't matter here.

Gate 6 forces that one call to return 1. Notes:

- **When it takes effect:** the screen is built lazily, by `ToyboxListDataProvider::createContentViewForClass`,
  when Boombox is first opened in a UI session. The package binds the patched library, so the switch shows
  once the screen is next built (at the latest after a UI restart). With the live `.sh`, apply it before
  opening Boombox after a UI restart.
- **What the switch does:** it sets `GUI_disableHorn`. libQtCarVAPI sends that to the body controllers
  as bit 2 of byte 0x8c in `VehicleControlMessage2`, with no country check on the MCU side, and they
  silence the real horn so only the Boombox sound plays. Gate 5 keeps it set in Drive. Whether the body
  controller honours the bit on a DE-coded car isn't visible in the MCU firmware: test it (parked).
- **Safety:** with Replace Horn on, the car has no regular horn, including while driving. A working horn
  is required equipment in the EU, and road safety is why Tesla only offers this in North America.
- **Builds:** the site is optional. It matches 2026.8.6 (`0x2727019`) and 2026.38.6 (`0x2f44af9`). In 38.6
  the dropdown table moved inside the object (`lea r12,[rbx+0x8e8]` instead of `+0x860`), so the pattern
  wildcards that displacement (v1.1.1); `canSuppressHorn()` there is still exactly the US/MX/CA check.
  Gates 1-5 also match 38.6, each patched call resolving to the same function as on 2026.8.6
  (`isCarParked`, `customPedestrianWarningSoundAllowed`, `inDrivingGear` x2, and the same
  `factoryLowHornNoiseModeActive` branch for gate 5). `horn-in-drive.sh` remains 2026.8.6-only.

## Gates 7-8: the toggles switch themselves off on every UI restart (added 2026-11-24, v1.2.0)

Reported on 2026.38.6: Horn Sound and Replace Horn come back unchecked "after lock/unlock". Stock
firmware does this in **both** builds. `CenterDisplay::initManagers` calls `ExternalSpeakerManager::enable()`
at every UI start, and that does
`dvConnect(GUI_factoryMode, this, "updateFactoryLowHornNoiseAreaMode()", true, …)`. The `true` makes
`DataValue::dvConnect` (libQtCarUtils) invoke the slot at once. The same slot also runs on every
`LocationManager` `regionEntered`/`regionExited` (any geofence: HomeLink, mirrors, suspension, automations)
and whenever factory mode changes. Outside the factory "low horn noise" mode it sets `GUI_disableHorn`
and `GUI_pesHornSoundEnabled` to false and logs `[ExternalSpeakerManager] INFO Exiting factory low horn
noise mode`, so the Horn Sound setting is also saved as false.

On-car evidence (2026-11-24, read-only):
- QtCar ran through two sleep/wake cycles (06:43 and 08:51 PST) without restarting, and the
  slot's log line never appeared then. Lock, unlock and wake alone don't trigger it.
- UI restarts do happen often, e.g. when CoilShift packages are installed or changed. There were
  several at 03:49-03:54 PST, and QtCar restarted at 03:51:57.
- `gui/pes_horn_sound_enabled` in `/home/tesla/.Tesla/data/QtCarSettings.db` was saved as `false`.

Gates 7 and 8 neutralise the two `setValue(false)` calls in that branch and leave the log line in place.
The branch that turns the mode on (with `movb $0x1`, only in factory mode at the factory location)
is untouched, and a customer car never enters it. Both sites are optional and `patched: skip`.

**The package setting `keep_toggles`** (bool, default on; "Keep horn toggles on after a UI restart")
controls them. A byte rule can't switch a site on or off from a setting: `when` only tests symbols, and a
setting reaches a rule only as `params` text in a token. So the rule's `params.keepfn` maps the setting to
the call target, `e8{rel:plt:$keepfn}`:
- `true` → `_ZN16DataValueManager8instanceEv`, a static accessor that ignores rdi/rsi, returns the
  existing singleton and has no side effects (`enable()` itself calls it). The call stays 5 bytes, and
  nothing reads its result because `rax` is overwritten right after.
- `false` → the original `setValue`. The replacement then equals the find, so the site counts as
  already patched and is skipped. The change list is byte-identical to v1.1.1, with digests
  `441fc77698e220e6` (8.6) and `b0eabdeb759a0643` (38.6).

`DataValue::metaObject()` was the first candidate. It won't work: libQtCarGUI only reaches it through
vtables (`R_X86_64_64`) and it has no PLT stub. I haven't seen on the car whether flipping the setting
re-applies at once or needs a UI restart. What still resets:
- **Replace Horn** after a UI restart: `GUI_disableHorn` is a non-persisted DataValue in QtCar
  (`persistent=false`), so it starts false. That's stock in every build. Re-enable it after a restart.
- **Replace Horn** during a phone or video call (`ExternalSpeakerManager::inPhoneCallChanged`).
  That's stock and kept on purpose.
- **A USB horn sound** (a file in the stick's `Boombox/` folder) falls back to the first built-in
  sound (Fart). Each USB scan runs `ExternalSoundCollection::validateSavedSound`, and a scan made
  before the stick is mounted finds no USB files. That's stock and not patched.

## Files

- `horn-in-drive.sh` — live `/proc/mem` patcher (`apply` | `status` | `restore`; reverts on UI restart). Self-validates stock bytes before writing.
- `coilshift/io.github.ivan-prodanov.horn-in-drive/` — durable package (`coilshift.json` + `rules/horn_in_drive.json`). All 5 forms dry-run verified: unique symbol-scoped match, exact patch VA, exact stock bytes; `csdk rules test` on 2026.8.6 (2026-11-20) gives the same 5 offsets as `horn-in-drive.sh`. Built and checked by `tools/build.sh` (see the top-level README); the change-list digest for 2026.8.6 is in `rules-digests.tsv`.

---

## Reference: how the horn feature is wired (from local RE)

Not needed to apply the patch, but captures what the DataValue layer does (and the dead ends), so the
next person doesn't repeat them.

- The external-speaker feature is **6 "sound collections"** in
  `assets/external_speaker/externalspeakercollections.json`: `CustomLock`/`Horn`/`Santa`/`Emissions`/
  `BoomBox`/`Locate`. `ExternalSoundCollection::isEnabled()`/`setEnabled()` read/write a per-collection
  DataValue at `[collection+0x28]`; the binary's DV names map 1:1:
  - **Horn (title "Horn Sound"): enable `GUI_pesHornSoundEnabled`, chosen sound `GUI_pesTheChosenHorn`**
  - CustomLock ("Lock Sound"): `GUI_pesLockSoundEnabled` / `GUI_pesLock` (= ClownHorn)
  - Replace Horn (separate flag): `GUI_disableHorn`
  - Emissions `GUI_theChosenFart`, Santa `GUI_pesSantaMusic`, Locate `GUI_pesLocate`
- **ClownHorn is a Lock sound, not a Horn sound.** The Horn list is `[Fart, Posh, ValleyOfPlenty,
  Jordan, OldHorn, Applause, Tada, BaDumTis, DJ, LaCucaraca, USB]`; the real horn sound is
  `GUI_pesTheChosenHorn` (OldHorn).
- **Dead end — DataValue holds:** `GUI_disableHorn` is a global DV (`lv`/`sdv`/`set-local-dv`) and Drive
  resets it; `GUI_pesHornSoundEnabled` returns NOT FOUND on global `lv` / `ldvs` (it's CenterDisplay-local
  or screen-scoped, persisted as setting `gui/pes_horn_sound_enabled`). Holding these forces the toggles
  on rather than making them *interactive*, so it was the wrong approach for the goal — the byte patch is.
- **DV tooling on the car** (`/usr/local/bin`): `lv`/`sdv` (→ `com.tesla.Debug` D-Bus), `ldvs` (dump all
  6222 global DVs), `get-local-dv`/`set-local-dv` (→ `com.tesla.LocalDataValue`), `sync-settings`
  (persist via `http://localhost:4070/persistDataValues`).
