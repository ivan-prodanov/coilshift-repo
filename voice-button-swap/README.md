# voice-button-swap

Steering-wheel voice button: a **short press starts Grok**, a **long press starts the Tesla
voice assistant** (stock 2026.38.6 is the other way round). Only the press that *starts* an
assistant changes. Once one is open, the button behaves exactly as stock.

> **Status: built and offline-verified on 2026.38.6 and 2026.8.6 (2026-11-26); not yet tried on the car.**
> Durable form only: CoilShift package `io.github.ivan-prodanov.voice-button-swap` (byte rules,
> role `browser-ui`, `live: hot`). There is no `.sh` live patcher. 1.0.0 was 38.6-only; 1.1.0 adds
> 2026.8.6 and the owner's description.
> Design: [DESIGN.md](DESIGN.md). Plan: [PLAN.md](PLAN.md).

## Behaviour

"Idle" means no assistant card is open, no voice command waits for confirmation, and the
on-screen keyboard is down.

| Situation | Short press | Long press |
|---|---|---|
| Idle | **Grok** (swapped) | **Tesla assistant** (swapped) |
| Grok card open / Grok session active | closes Grok (stock) | mute toggle (stock) |
| Tesla assistant card open | stock | switches to Grok (stock) |
| Command waiting for confirmation | confirm (stock) | reject (stock) |
| Keyboard up | dictation (stock) | stock |
| Grok blocked (`GUI_assistantBlockedReason` 2 or 3) | Tesla assistant (stock) | Tesla assistant |
| Grok turned off | Tesla assistant (the hold handler's own fallback) | Tesla assistant |

The "Long press for Grok" hint on the Tesla assistant's card is hidden. It would contradict the new
mapping, though a long press inside that card still switches to Grok (stock).

## 2026.8.6

The same mechanism, with a smaller blob (`vbswap86`, assembled with `-DFW_2026_8_6`):
- 8.6 has no voice-command confirmation (no `GUI_voiceCommandApprovalState`), so that check is gone.
- 8.6's `voiceButtonHeld` has no `isAssistantVoiceAllowed()` gate, so the short press has no
  Grok-blocked check either, mirroring it. When Grok is on, the hold handler calls
  `VoiceControlHandler::grokModeEngaged` instead of a D-Bus `VoiceButtonHeld`; when Grok is off it
  falls back to `voiceButtonPressed`, as on 38.6.
- The same slot stubs (8.6 va `0x1edc130` short press, `0x1edc120` hold) and the same hint function
  (va `0x27f7070`, 136 bytes).

**Which blob goes where:** the SDK ignores `when` on a blob site. So each blob's `prologue` check
includes the hint function's `lea rdi,[rip+"grok"]` displacement, which differs between the builds.
The jump forms pick their blob with `when` (`CarAssistClient::asyncVoiceButtonHeld` exists only on
38.6, `VoiceControlHandler::onAudioIntensityLevel` only on 8.6).

**Offline check, 2026-11-26:** the 8.6 change list applied to a copy and disassembled. objdump's PLT
labels are right on 8.6, and I resolved them independently through `.rela.plt`. Both stubs reach the
right entry, the jumps reach `voiceButtonHeld`/`voiceButtonPressed`, and the four GOT slots are the
right DataValues.

## How the firmware picks the assistant (2026.38.6)

- `SteeringWheelControlsManager` connects the voice button's `SteeringWheelButtonListener`:
  `released()` → `VoiceControlHandler::voiceButtonPressed()`, `held()` → `voiceButtonHeld()`
  (`pressed()` and `releasedAfterHold()` go to empty slots). The connection is by slot name, so
  Qt calls them through `VoiceControlHandler::qt_static_metacall`, slot 11 and slot 12. Each slot is a
  tail jump: va `0x400b370` → `voiceButtonPressed@plt`, `0x400b360` → `voiceButtonHeld@plt`.
- Both handlers check the pending confirmation first (a press confirms, a hold rejects), then the phone,
  dog mode and the voice-rec gates. Then they make a D-Bus call to `car-assist`:
  `CarAssistClient::asyncVoiceButtonPressed` / `asyncVoiceButtonHeld`. `voiceButtonHeld` falls back to
  `voiceButtonPressed` when Grok is off, and ignores the hold when `isAssistantVoiceAllowed()` is false.
- `car-assist` (Go, `main.go:355/394`) makes the choice. `VoiceButtonPressed` closes an active Grok card or
  runs `handleButtonPressAction(false)`: the Tesla assistant, keyboard dictation, barge-in.
  `VoiceButtonHeld` toggles mute when Grok is active, else `GrokModeEngaged`, which runs
  `handleButtonPressAction(true)`. CoilShift byte rules only cover the UI libraries, so the patch
  can't go there.

## The patch (3 changes)

| # | Where | What |
|---|---|---|
| 1 | `SteeringWheelLongPressVoiceHintView::currentLongPressVoiceAction` (va `0x3064770`, 128 bytes) | replaced by the 126-byte blob from [`blob/vbswap.S`](blob/vbswap.S): offset 0 is `xor eax,eax; ret` (the hint is now always *none*), then the `press` entry (+3), the `held` entry (+0x25) and the idle test |
| 2 | `qt_static_metacall` slot 11 stub (va `0x400b370`) | `jmp voiceButtonPressed@plt` → `jmp blob+press` |
| 3 | `qt_static_metacall` slot 12 stub (va `0x400b360`) | `jmp voiceButtonHeld@plt` → `jmp blob+held` |

- **`press`:** if Grok is blocked, `jmp voiceButtonPressed`. Otherwise, if idle, `jmp voiceButtonHeld`,
  else `jmp voiceButtonPressed`.
- **`held`:** if idle, `jmp voiceButtonPressed`, else `jmp voiceButtonHeld`.
- **The idle test reads:** `GUI_voiceCommandApprovalState` (valid and == 1 means a confirmation is pending),
  then `GUI_carAssistViewOpen` (Tesla assistant card), `GUI_assistantViewOpen` (Grok card),
  `GUI_assistantVoiceActive` (Grok session) and `GUI_voiceRecAsPlainText` (keyboard up). Each is reached
  through its GOT slot.
- **Field layout:** bools at +0x49; the enum's valid bit is +0x34 bit 0 and its value +0x4c. That's what
  the stock code reads.
- **No lock:** the stock code reads these under a `DataValue::ConditionalLocker`. The blob doesn't. Each read
  is one aligned load, on the UI thread, which is the thread that writes these values.
- **Registers:** only `rax` and the flags change; `rdi` (`this`) and the stack reach the stock handler as
  `qt_static_metacall` left them.
- **Who's affected:** only the wheel button goes through slots 11 and 12. Direct C++ callers of the handlers
  are untouched: the keyboard mic (`KeyboardViewController::keyboardVoiceRecStarted`), the media settings
  button, `ClusterUI`'s `processButtonAction` and the `voiceButtonHeld` fallback.

**Why the blob sits on the hint function:** `libQtCarGUI` has no int3 padding (it's padded with multi-byte
NOPs), so `int3_run` placement is impossible. The hint function is the one piece of code the package wants
dead anyway, and its only caller is `SteeringWheelLongPressVoiceHintView::updateView`.

## Building and checking

```bash
python3 -I voice-button-swap/blob/gen.py           # assemble vbswap.S (clang) and write the rule set
python3 -I voice-button-swap/blob/gen.py --check   # exit 1 if the rule set is stale
tools/build.sh                                     # lint, csdk test, rules test, digests
```

`gen.py` takes the blob hex, the fixups (`plt:` for `voiceButtonPressed` / `voiceButtonHeld`, `got:` for
the six DataValues) and the entry offsets from the assembled object. Any other relocation is refused.
Offline check, 2026-11-26: the change list applied to a copy of the 38.6 library and disassembled.
Both stubs land on the right entry. Each jump reaches the right PLT stub and each load the right GOT slot,
resolved with `fw at`, since objdump's PLT labels are one entry off on this build.

## On the car (to do)

Not tested on the car yet. Test while parked:
1. Short press: Grok opens. Short press again: Grok closes. Long press while Grok is open: mute.
2. Long press: the Tesla assistant opens, without the "Long press for Grok" hint.
3. Ask the Tesla assistant to lock the doors: a short press confirms.
4. Keyboard open: a short press dictates.
