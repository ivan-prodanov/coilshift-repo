/*
 * voice-button-swap: the code blob the rule set places in libQtCarGUI (2026.38.6).
 *
 * Where it lives: over SteeringWheelLongPressVoiceHintView::currentLongPressVoiceAction()
 * (128 bytes). Its first three bytes stay that function, now `return 0` (LongPressVoiceAction
 * none), which hides the card's "Long press for Grok" hint. The library has no int3 padding,
 * so this function, which the package wants dead anyway, holds the rest of the code.
 *
 * What it does: VoiceControlHandler::qt_static_metacall dispatches the steering-wheel
 * button's slots with tail jumps: slot 11 (released, a short press) -> voiceButtonPressed(),
 * slot 12 (held) -> voiceButtonHeld(). The rule set points those two jumps at `press` and
 * `held`. rdi (the handler's `this`) and the stack pass through untouched; only rax and the
 * flags change before the tail jump.
 *
 * Idle = no assistant card open, no voice command waiting for confirmation, keyboard down.
 * When idle the gestures swap: a short press runs the stock hold handler (Grok) and a hold
 * runs the stock press handler (old assistant). When busy each gesture keeps its own stock
 * handler, so closing/muting Grok, confirm/reject and dictation are unchanged. A short press
 * also stays stock while Grok is blocked (GUI_assistantBlockedReason 2 or 3), where the hold
 * handler would ignore it.
 *
 * DataValue layout, as the stock code reads it: a bool's value at +0x49; an enum's valid bit
 * at +0x34 bit 0 and its value at +0x4c. The reads are unlocked single loads on the UI
 * thread, which is the thread that writes these values.
 *
 * Builds: assembled twice. The 2026.38.6 blob is the default. With -DFW_2026_8_6 the
 * confirmation and Grok-blocked checks are left out: 2026.8.6 has no voice-command
 * confirmation (no GUI_voiceCommandApprovalState) and its hold handler has no
 * isAssistantVoiceAllowed() gate, so the short press mirrors it.
 *
 * blob/gen.py assembles this file and writes the rule set; never hand-edit the hex.
 */
        .intel_syntax noprefix
        .text

/* offset 0: currentLongPressVoiceAction() now always returns none (0) */
hint:
        xor     eax, eax
        ret

/* entry "press": the button was released after a short press */
        .globl  press
press:
#ifndef FW_2026_8_6
        mov     rax, qword ptr [rip + GUI_assistantBlockedReason@GOTPCREL]
        mov     eax, dword ptr [rax + 0x4c]
        sub     eax, 2
        cmp     eax, 1
        jbe     1f                      /* Grok blocked: keep the stock short press */
#endif
        call    idle
        jz      _ZN19VoiceControlHandler15voiceButtonHeldEv     /* idle: Grok */
1:      jmp     _ZN19VoiceControlHandler18voiceButtonPressedEv  /* stock */

/* entry "held": the button has been held past the hold timeout */
        .globl  held
held:
        call    idle
        jz      _ZN19VoiceControlHandler18voiceButtonPressedEv  /* idle: old assistant */
        jmp     _ZN19VoiceControlHandler15voiceButtonHeldEv     /* stock */

/* ZF = 1 when idle: nothing waits for confirmation, no assistant card is open, keyboard down */
idle:
#ifndef FW_2026_8_6
        mov     rax, qword ptr [rip + GUI_voiceCommandApprovalState@GOTPCREL]
        test    byte ptr [rax + 0x34], 1                                   /* valid? */
        jz      2f
        cmp     dword ptr [rax + 0x4c], 1                                  /* 1 = pending */
        je      3f
#endif
2:      mov     rax, qword ptr [rip + GUI_carAssistViewOpen@GOTPCREL]      /* old assistant card */
        cmp     byte ptr [rax + 0x49], 0
        jne     3f
        mov     rax, qword ptr [rip + GUI_assistantViewOpen@GOTPCREL]      /* Grok card */
        cmp     byte ptr [rax + 0x49], 0
        jne     3f
        mov     rax, qword ptr [rip + GUI_assistantVoiceActive@GOTPCREL]   /* Grok session */
        cmp     byte ptr [rax + 0x49], 0
        jne     3f
        mov     rax, qword ptr [rip + GUI_voiceRecAsPlainText@GOTPCREL]    /* keyboard up: dictation */
        cmp     byte ptr [rax + 0x49], 0                                   /* ZF = keyboard down */
        ret
3:      or      al, 1                                                      /* busy: ZF = 0 */
        ret
