# Voice button swap: design (approved 2026-11-26)

**Intent.** On the steering-wheel voice button, a short press should start Grok and a long
press the old (Tesla) assistant: the reverse of stock 2026.38.6. The owner uses Grok more.

**Goals.** Only the gesture that *starts* an assistant flips. **Anti-goals** (owner, 2026-11-26):
a plain swap is not acceptable, because it would also swap closing/muting Grok, confirming vs
rejecting a pending voice command, and keyboard dictation. Those stay stock.

**Rule.** The button is *idle* when no assistant card is open, no voice command waits for
confirmation and the on-screen keyboard is down:

| Situation | Short press | Long press |
|---|---|---|
| Idle | Grok (swapped) | old assistant (swapped) |
| Grok card open / Grok active | closes Grok (stock) | mute toggle (stock) |
| Old assistant card open | stock | Grok (stock) |
| Confirmation pending | confirm (stock) | reject (stock) |
| Keyboard up | dictation (stock) | stock |
| Grok blocked (`GUI_assistantBlockedReason` 2 or 3) | old assistant (stock) | old assistant (swapped) |
| Grok turned off | old assistant (stock handler fallback) | old assistant |

The card's "Long press for Grok" hint is hidden (owner's pick): it would contradict the new mapping.

**Approaches considered.**
1. *Plain swap* of the two slot jumps in `VoiceControlHandler::qt_static_metacall`: rejected
   by the owner (in-session controls, confirmation and dictation swap too).
2. *Patch `car-assist`* (Go; it maps `VoiceButtonPressed` → `handleButtonPressAction(false)`,
   `VoiceButtonHeld` → `GrokModeEngaged`): CoilShift byte rules only cover the UI libraries,
   so it would need an out-of-band bind mount. Rejected.
3. **Chosen:** in `libQtCarGUI`, point the short-press (slot 11) and hold (slot 12) jumps of
   `VoiceControlHandler::qt_static_metacall` at a code blob that tests the idle rule and
   tail-jumps to the other gesture's stock handler when idle, else to its own. Each stock
   handler keeps all its own gates (phone, dog mode, voice-rec allowed, Grok on/off). Direct
   C++ callers (keyboard mic, media settings, `processButtonAction`) are untouched.

**Constraints.** CoilShift byte rules v2, role `browser-ui`, `live: hot`; build 2026.38.6
(2026.8.6 is frozen). The blob reads DataValue fields the stock code reads under a lock
(bool at +0x49; enum valid bit +0x34&1, value +0x4c) without the lock: single aligned
loads on the UI thread that writes them.

**Left to the builder.** Blob placement (`int3_run`), the exact site patterns, the hint site.
