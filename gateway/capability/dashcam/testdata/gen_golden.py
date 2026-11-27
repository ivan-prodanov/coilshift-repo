#!/usr/bin/env python3
"""Regenerate golden_events.json from inventory.json with the laptop viewer's own build_events.

    DASHCAM_CACHE=$(mktemp -d) python3 -I gen_golden.py <path/to/re/bridge/camera/dashcam_viewer.py>

It renders inventory.json as the car's INVENTORY_CMD output (E|/J|/F|/R| lines, the shell's `tr -d '\\n\\r\\t'`
on event.json), replaces dashcam_viewer.ssh_run with a function returning that text, calls build_events()
unchanged and writes its JSON. Only the inventory transport differs on the gateway, so the Go port must match it.
"""
import importlib.util
import json
import pathlib
import sys

here = pathlib.Path(__file__).resolve().parent
spec = importlib.util.spec_from_file_location("dashcam_viewer", sys.argv[1])
dv = importlib.util.module_from_spec(spec)
spec.loader.exec_module(dv)

inv = json.loads((here / "inventory.json").read_text())
lines = []
for d in inv["dirs"]:
    lines.append(f"E|{d['type']}|{d['folder']}")
    if d["event"] is not None:
        lines.append("J|" + d["event"].replace("\n", "").replace("\r", "").replace("\t", ""))
    for f in d["files"]:
        lines.append(f"F|{f['name']}|{f['size']}")
for f in inv["recent"]:
    lines.append(f"R|{f['name']}|{f['size']}")
text = "\n".join(lines) + "\n"

dv.ssh_run = lambda remote, timeout=30: (True, text)
events = dv.build_events()
(here / "golden_events.json").write_text(json.dumps(events, indent=1, ensure_ascii=False) + "\n")
print(f"{len(events)} events")
