#!/usr/bin/env python3
"""Assemble blob/vbswap.S and write the voice-button-swap byte rule set.

    python3 -I voice-button-swap/blob/gen.py [--check]

The rule set's blob hex and fixups come only from here, so the assembly stays the reviewed
source. --check exits 1 if the rule set on disk differs from what this would write.
"""
import json
import os
import struct
import subprocess
import sys
import tempfile

HERE = os.path.dirname(os.path.abspath(__file__))
SRC = os.path.join(HERE, "vbswap.S")
PKG = os.path.join(HERE, "..", "coilshift", "io.github.ivan-prodanov.voice-button-swap")
OUT = os.path.join(PKG, "rules", "voice_button_swap.json")

METACALL = "_ZN19VoiceControlHandler18qt_static_metacallEP7QObjectN11QMetaObject4CallEiPPv"
PRESSED = "_ZN19VoiceControlHandler18voiceButtonPressedEv"
HELD = "_ZN19VoiceControlHandler15voiceButtonHeldEv"
PRESS_STARTED = "_ZN19VoiceControlHandler23voiceButtonPressStartedEv"
RELEASED_AFTER_HOLD = "_ZN19VoiceControlHandler28voiceButtonReleasedAfterHoldEv"
HINT = "_ZN35SteeringWheelLongPressVoiceHintView27currentLongPressVoiceActionEv"
ONLY_38_6 = "_ZN15CarAssistClient20asyncVoiceButtonHeldEv"       # 2026.38.6 defines it, 2026.8.6 does not
ONLY_8_6 = "_ZN19VoiceControlHandler21onAudioIntensityLevelEid"  # 2026.8.6 defines it, 2026.38.6 does not
# currentLongPressVoiceAction starts push r14; push rbx; lea rdi,[rip+"grok"]. The lea's displacement
# differs per build, so each blob's prologue check pins it to its own build: the SDK ignores `when` on
# a blob site, and without this both blobs would land on 2026.38.6.
HINT_PROLOGUE = {"vbswap": "415653488d3dd95520ff", "vbswap86": "415653488d3d160e5101"}
NOP11 = "66662e0f1f840000000000"  # the padding between qt_static_metacall's tail-jump stubs

R_X86_64_PC32, R_X86_64_PLT32 = 2, 4
R_X86_64_GOTPCREL, R_X86_64_GOTPCRELX, R_X86_64_REX_GOTPCRELX = 9, 41, 42


def assemble(defines=()):
    with tempfile.TemporaryDirectory() as tmp:
        obj = os.path.join(tmp, "vbswap.o")
        subprocess.run(["clang", "-target", "x86_64-linux-gnu", *("-D" + d for d in defines), "-c", SRC, "-o", obj],
                       check=True)
        return open(obj, "rb").read()


def parse(elf):
    shoff, = struct.unpack_from("<Q", elf, 0x28)
    shentsize, shnum, shstrndx = struct.unpack_from("<HHH", elf, 0x3A)
    secs = [struct.unpack_from("<IIQQQQIIQQ", elf, shoff + i * shentsize) for i in range(shnum)]
    shstr = secs[shstrndx]

    def name(off, tab):
        end = elf.index(b"\0", tab[4] + off)
        return elf[tab[4] + off:end].decode()

    by_name = {name(s[0], shstr): s for s in secs}
    text = by_name[".text"]
    code = elf[text[4]:text[4] + text[5]]
    symtab, strtab = by_name[".symtab"], secs[by_name[".symtab"][6]]
    syms = []
    for i in range(symtab[5] // 24):
        st_name, st_info, st_other, st_shndx, st_value, st_size = struct.unpack_from("<IBBHQQ", elf, symtab[4] + i * 24)
        syms.append((name(st_name, strtab), st_shndx, st_value))
    rela = by_name.get(".rela.text")
    relocs = []
    if rela:
        for i in range(rela[5] // 24):
            r_off, r_info, r_add = struct.unpack_from("<QQq", elf, rela[4] + i * 24)
            relocs.append((r_off, r_info & 0xFFFFFFFF, syms[r_info >> 32][0], r_add))
    entries = {n: v for n, sh, v in syms if n in ("press", "held")}
    return code, relocs, entries


def blob(name, defines, size):
    """One assembled variant of vbswap.S as a rule-set blob over currentLongPressVoiceAction."""
    code, relocs, entries = parse(assemble(defines))
    if code[:3] != bytes.fromhex("31c0c3"):
        sys.exit("the blob must start with xor eax,eax; ret (the hint function's new body)")
    if len(code) > size:
        sys.exit(f"{name} is {len(code)} bytes; currentLongPressVoiceAction holds {size}")
    fixups = []
    for off, typ, sym, add in relocs:
        if typ in (R_X86_64_PLT32, R_X86_64_PC32) and sym in (PRESSED, HELD):
            to = "plt:" + sym
        elif typ in (R_X86_64_GOTPCREL, R_X86_64_GOTPCRELX, R_X86_64_REX_GOTPCRELX) and sym.startswith("GUI_"):
            to = "got:" + sym
        else:
            sys.exit(f"unexpected relocation {typ} to {sym} at {off:#x}")
        fixups.append({"at": off, "kind": "pc32", "to": to, "addend": add})
    return {"name": name, "hex": code.hex(), "place": "symbol_start", "symbol": HINT,
            "prologue": HINT_PROLOGUE[name], "fixups": fixups,
            "entries": {"press": entries["press"], "held": entries["held"]}}


def rule_set():
    plt = lambda s: "{rel:plt:%s}" % s
    # (blob name, cpp defines, currentLongPressVoiceAction st_size, a symbol only that build defines)
    builds = [("vbswap", (), 128, ONLY_38_6), ("vbswap86", ("FW_2026_8_6",), 136, ONLY_8_6)]
    short_forms = [{"when": {"symbol": only}, "find": f"e9{plt(PRESSED)} {NOP11} e9{plt(PRESS_STARTED)}",
                    "span": [0, 5], "replace": "e9{rel:blob:%s+press}" % name} for name, _, _, only in builds]
    hold_forms = [{"when": {"symbol": only}, "find": f"e9{plt(RELEASED_AFTER_HOLD)} {NOP11} e9{plt(HELD)}",
                   "span": [16, 21], "replace": "e9{rel:blob:%s+held}" % name} for name, _, _, only in builds]
    return {
        "schema": 2,
        "role": "browser-ui",
        "live": "hot",
        "requires_symbols": [METACALL, PRESSED, HELD, HINT],
        "sites": [
            # optional: each blob places only on its own build (see HINT_PROLOGUE)
            *({"name": f"blob/vbswap.S ({label}) over currentLongPressVoiceAction: returns none (hides 'Long press for Grok'), then the idle test and the two gesture entries",
               "blob": name, "required": False}
              for (name, _, _, only), label in zip(builds, ("2026.38.6", "2026.8.6"))),
            {"name": "short press (qt_static_metacall slot 11, voiceButtonPressed) goes through the blob's press entry",
             "scope": {"symbol": METACALL}, "patched": "refuse", "forms": short_forms},
            {"name": "hold (qt_static_metacall slot 12, voiceButtonHeld) goes through the blob's held entry",
             "scope": {"symbol": METACALL}, "patched": "refuse", "forms": hold_forms},
        ],
        "blobs": [blob(name, defines, size) for name, defines, size, _ in builds],
    }


def main():
    text = json.dumps(rule_set(), indent=2) + "\n"
    if "--check" in sys.argv:
        same = os.path.exists(OUT) and open(OUT).read() == text
        print("rule set is current" if same else "rule set is stale: run gen.py")
        sys.exit(0 if same else 1)
    os.makedirs(os.path.dirname(OUT), exist_ok=True)
    open(OUT, "w").write(text)
    print("wrote", os.path.relpath(OUT))


if __name__ == "__main__":
    main()
