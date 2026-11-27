# fart-in-drive

Emissions **fart button usable while in gear**, with the fart routed to the
**external** speaker (PES) in motion. **Verified live on-car 2026-11-08** — all
three gates (button stays enabled in gear + external fart plays).

Builds: libQtCarGUI **2026.8.6-amd** (verified on-car), **2026.38.3.2** (derived 2026-11-20) and
**2026.38.6** (checked 2026-11-21); both newer builds are offline-verified only, **not yet run on a car**. Offsets are per-build;
`fart-in-drive.sh` detects the build from the bytes in memory and refuses one it doesn't know.

## Gates (3) — 2026.8.6-amd

| # | Function @ site | Reads | Patch | Effect |
|---|---|---|---|---|
| 1 | `FartModeView::updateInfo` @ `0x26fde4d` | `VehicleUtils::customPedestrianWarningSoundAllowed` (0x145d820) → enabled flag | → `b801000000` (`mov eax,1`) | un-greys the button (plays **interior** fart in gear) |
| 2 | `FartModeManager::playExternalFart` @ `0x2701094` | `isCarParked` (0x14736e0) | → `b801000000` | plays **external** (PES) fart, not interior |
| 3 | `ExternalSoundPlayer::playSound` @ `0x2708e29` | `inDrivingGear` (0x147d720) | → `31c0909090` (force not-in-gear) | external speaker actually emits in motion |

## 2026.38.6 (BuildID `dffec28d…`)

Same as 2026.38.3.2 below: same three gates and callees (each patched call resolved to
`customPedestrianWarningSoundAllowed`, `isCarParked` followed by the near `je`, and `inDrivingGear`),
the same 16-byte PLT stubs, and vaddr = file offset + 0x1000. Sites (vaddr): `0x2f070ed`, `0x2f0c5a9`,
`0x2f19329`, stock `e83eb155ff` / `e8424a55ff` / `e8a27c54ff`. The rule set matches unchanged (digest
`a2502cb5be941f8c`); `fart-in-drive.sh` has a `ROWS_2026_38_6` table and writes identical bytes.

## 2026.38.3.2 (BuildID `0a199995…`; the dump's library has since been removed)

Same three gates, same callees, same patches. Diffing every gear/park/speed call on the
path (`updateInfo`, `playExternalFart`, `ExternalSpeakerManager::playSoundFromType`,
`ExternalSoundPlayer::playSound`, `playInteriorFart`) against 2026.8.6 shows nothing new
that gates on driving.

| # | Site (vaddr) | stock → patched |
|---|---|---|
| 1 | `0x2e95f2d` | `e82ef255ff` → `b801000000` |
| 2 | `0x2e9b3e9` | `e8628b55ff` → `b801000000` |
| 3 | `0x2ea8169` | `e8c2bd54ff` → `31c0909090` |

What changed, and what to know when re-deriving:

- **Why the old rule set refused gate 2:** after `isCarParked` the compiler now emits a near
  `je` (`0f 84 rel32`) instead of a short one (`74 rel8`). The logic is unchanged: not
  parked → tail-jump to `playInteriorFart`.
- **New, deliberately not patched:** `playExternalFart` and `playInteriorFart` now also bail
  out while `PhoneManager::inCall()` (on top of the old `GUI_videoCallActive`), and
  `playExternalFart` takes a separate branch while `ECALL_cancellationState` bit 0 is set
  (emergency call). Neither is a driving gate, so the fart still won't play during a call.
- **vaddr ≠ file offset:** this build's code segment is mapped 0x1000 above its file offset,
  so `csdk rules test` prints `0x2e94f2d` / `0x2e9a3e9` / `0x2ea7169` (file offsets). The live
  patcher needs the vaddrs above.
- **objdump mislabels the PLT here:** stubs are 16 bytes (`mov r11d, idx; jmp [got]`), so
  `objdump` names every call target "<previous stub>@plt+0xa". The real callee is the
  label at target + 6.

## coilshift forms

`coilshift/io.github.ivan-prodanov.fart-in-drive/rules/fart_in_drive.json`. Each site names
its callee with `{rel:plt:…}` instead of a `????????` wildcard, so a match proves the call
really goes to that function. The branch opcode stays in each pattern, because a `je`
there is what makes forcing the result correct:

| Gate | find | span | replace |
|---|---|---|---|
| updateInfo | `bf01000000 e8{rel:plt:…customPedestrianWarningSoundAllowedEb} 89c3` | `[5,10)` | `b801000000` |
| playExternalFart | `31f631ff e8{rel:plt:…isCarParkedEbb} 84c0 74` (2026.8.6) or `… 84c0 0f84` (2026.38.3.2) | `[4,9)` | `b801000000` |
| playSound | `bf01000000 e8{rel:plt:…inDrivingGearEb} 84c0 74` | `[5,10)` | `31c0909090` |

Each `find` matches exactly once within its scoped symbol. **Package built + dry-run
verified 2026-11-08.** On 2026.8.6 the change-list digest is unchanged by the 2026.38.3.2
rewrite (`24b9444ebe50e823`). On 2026.38.3.2 the rule set and `fart-in-drive.sh` write
byte-identical changes, checked on a memory image of the dumped lib. Both builds' digests are in
`rules-digests.tsv`, and `tools/build.sh` checks them on every run.

## Files

- `fart-in-drive.sh` — live `/proc/mem` patcher (reverts on UI restart; re-run `apply`). One
  row table per build; `status` prints the detected build. A process whose gate bytes match
  no known build is refused untouched.
- `coilshift/io.github.ivan-prodanov.fart-in-drive/` — package source (`coilshift.json` + `rules/fart_in_drive.json`); `tools/build.sh` builds it into `dist/public/`.
  CoilShift lists it as **Fart & Furious**, with the summary "Bringing emissions back to
  EVs. …" (v1.1.0). The app draws `summary` as one plain paragraph: newlines collapse and
  there's no separate tagline style, so the copy is written to read as one paragraph.
