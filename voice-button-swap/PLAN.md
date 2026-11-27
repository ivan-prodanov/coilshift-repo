# Voice button swap Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** A CoilShift package that makes a short press of the wheel voice button start Grok and a long press the old assistant when no assistant session is open, everything else stock.

**Architecture:** One byte rule set on role `browser-ui` (`libQtCarGUI.so.1.0.0`): a code blob (`int3_run`) decides idle vs busy from five DataValues, and the slot-11/12 tail jumps of `VoiceControlHandler::qt_static_metacall` are pointed at its two entries. A generator assembles the blob source with clang and writes the blob hex and fixups into the rule set, so the assembly stays the reviewed source.

**Tech Stack:** x86-64 assembly (clang, Intel syntax), Python 3 generator, csdk 0.1.2 (`tools/build.sh`).

**Spec:** `voice-button-swap/DESIGN.md`

## Global Constraints

- Package id `io.github.ivan-prodanov.voice-button-swap`, version `1.0.0`, `byte_rules:browser-ui`, `live: hot`.
- Build 2026.38.6 only (lib sha256 `1cb38fbe…df4d6`); 2026.8.6 must refuse, never write.
- DataValue layout (from stock reads): bool value at `+0x49`; enum valid bit `+0x34 & 1`, value `+0x4c`.
- No VIN, tailnet name or local path in anything that is published.

## Review Focus

- Half-applied swap (one jump patched, the other not): all three swap sites required, so a mismatch refuses the whole set.
- Blob clobbering `rdi` (the handler's `this`): the idle test must only touch `rax` and flags.
- Grok blocked (`GUI_assistantBlockedReason` 2 or 3): short press must stay stock, else it would do nothing.
- Re-applying on an already patched library: `csdk test` `apply,apply` must equal one apply.
- Another build: `csdk rules test --expect-refusal` on 2026.8.6.

---

### Task 1: Blob source, generator and rule set

**Files:**
- Create: `voice-button-swap/blob/vbswap.S`, `voice-button-swap/blob/gen.py`
- Create: `voice-button-swap/coilshift/io.github.ivan-prodanov.voice-button-swap/rules/voice_button_swap.json` (written by `gen.py`)

**Interfaces:**
- Produces: blob `vbswap` with entries `press` (offset 0) and `held`; external refs only to PLT `voiceButtonPressed` / `voiceButtonHeld` and GOT `GUI_carAssistViewOpen`, `GUI_assistantViewOpen`, `GUI_assistantVoiceActive`, `GUI_voiceRecAsPlainText`, `GUI_voiceCommandApprovalState`, `GUI_assistantBlockedReason`.

- [ ] **Step 1:** Write `vbswap.S`: `press` = if (not blocked and idle) jmp `voiceButtonHeld` else jmp `voiceButtonPressed`; `held` = if idle jmp `voiceButtonPressed` else jmp `voiceButtonHeld`; helper returns `al`.
- [ ] **Step 2:** `gen.py` assembles with `clang -target x86_64-linux-gnu -c`, reads `.text` and its RELA entries, maps `R_X86_64_PLT32` → `plt:NAME`, `GOTPCREL*` → `got:NAME` (pc32, addend from the reloc), refuses any other reloc, and writes the rule set: blob site + slot-11 site + slot-12 site (required) + hint site (`currentLongPressVoiceAction` → `assistantEnabled` call becomes `xor eax,eax`; optional).
- [ ] **Step 3:** Run `tools/bin/csdk rules test <rules> ../tesla-dump/2026.38.6/usr/tesla/UI/lib/libQtCarGUI.so.1.0.0`. Expected: 4 changes, a digest; and `--expect-refusal` against the 2026.8.6 lib passes.

### Task 2: Package, docs, build gates

**Files:**
- Create: `voice-button-swap/coilshift/io.github.ivan-prodanov.voice-button-swap/coilshift.json`, `voice-button-swap/README.md`
- Modify: `README.md` (feature table), `rules-digests.tsv` (38.6 row), `tools/build.sh` only if it needs the 8.6 refusal recorded

- [ ] **Step 1:** Manifest (schema 1, title "Voice Button Swap", summary in the owner's terms).
- [ ] **Step 2:** `tools/build.sh --record`, review the digest diff (only the new row), then `tools/build.sh` passes.
- [ ] **Step 3:** Apply the change list to a copy of the 38.6 lib and disassemble both entries and the blob: each jump lands on the blob, the blob's fixups land on the right PLT stubs and GOT slots.

### Task 3: Publish

- [ ] **Step 1:** `tools/build.sh --site`, `tools/publish.sh` (public `ivan-prodanov/coilshift-repo`).
- [ ] **Step 2:** Commit on a branch, push it to the private `ivan-prodanov/tesla-fw-patches`, fast-forward `main` like the previous release.
