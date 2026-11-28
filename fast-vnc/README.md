# fast-vnc

**The touchscreen over VNC, fast enough to use.** CoilShift's built-in VNC (`dev.coilshift.vnc`) runs
at about 0.5 updates per second. Fast VNC runs the same x11vnc from the firmware with one different
way of reading the screen, which is about 4 times faster. It also has touch, a visible pointer and an
optional smaller picture.

**Status:** ✅ **verified on the car** 2026-11-28 (2026.38.6): its service script, run by hand the
way CoilShift runs it, served the screen on 5901, and the owner confirmed taps and swipes from
TigerVNC. Also lint, `csdk test` and `test/run.sh`. Not yet installed through CoilShift.

## Install and connect

Install **Fast VNC** in CoilShift. It needs CoilShift's Tailscale, and it listens only on the car's
Tailscale address, **port 5901**, next to the built-in VNC on 5900. You can keep both or remove the
built-in one.

- **Screen Sharing (macOS):** `vnc://<car's Tailscale name>:5901`, then View > Scale to fit.
- **TigerVNC** (faster, because it can use JPEG): `<car's Tailscale name>::5901`. TigerVNC can't
  scale the picture, so on a 16-inch MacBook set **Picture size** to 1728x1080 and use full screen
  (F8 > Full screen).

Settings:

| Setting | Default | |
|---|---|---|
| VNC password | `coilshift` | VNC checks only the first 8 characters. |
| Picture size | Full (2560x1600) | 1728x1080 fills a 16-inch MacBook; Half (1280x800) for small screens or slow links. |

The status line under the package says `Serving the screen at <size> on port 5901 over Tailscale.`
once it runs. Until then it says what it's waiting for (Tailscale, the display, the port).

## Measured (2026.38.6, Mac to car over Tailscale, direct path)

| | updates/s | time to each update |
|---|---|---|
| Built-in VNC, Screen Sharing | 0.4 | 2.6 s |
| Fast VNC, Screen Sharing | 1.9 (2.6 at half size) | 0.4 s |
| Fast VNC, TigerVNC | 4.4-5.6 | 0.17 s |

While a viewer is connected, x11vnc uses about 80% of one of the car's 8 cores (the car goes from
about 25% to 38% busy). With no viewer it uses nothing.

## How it works

The X server is `amdgpu` with glamor, and Tesla's Xorg config turns MIT-SHM off. x11vnc's default
change detection reads many single lines and tiles of the screen through XGetImage, and on glamor
each read is a GPU readback that waits on the GPU. One whole-screen read takes about 0.1 s, but the
many small ones add up to 2-3 s per update, with x11vnc almost idle. `-snapfb` reads the whole screen
in one request and compares the copy in memory.

macOS Screen Sharing only offers Zlib to a non-Apple server (no Tight/JPEG), about 3.5 MB per screen
of change. That and the car's LTE link limit it to about 2 updates/s. TigerVNC uses Tight JPEG, about
300 KB per screen. Either way, about 5 updates/s is x11vnc's limit here: its main thread spends that
time on the whole-screen copy.

- `bin/fastvnc.sh` (the service): waits for the display, the touch device, Tailscale and a free
  port 5901; copies `/usr/bin/x11vnc` into its run dir (Tesla's AppArmor profile for that path is
  meant for Tesla's own sandboxed screenshare); writes the password file; runs
  `x11vnc -snapfb -cursor arrow -listen <tailscale ip> -rfbport 5901 [-scale ...] -pipeinput touch.sh`,
  with the rest of the flags as the built-in VNC (`-noshm -noxdamage -nowf -threads -nokeyboard`).
- `bin/touch.sh`: x11vnc's `-pipeinput` lines (`Pointer <client> <x> <y> <mask> <hint>`) become one
  finger on `/dev/input/touch`, the evdev node of Tesla's touch-gateway (multitouch tracking id, X, Y,
  SYN_REPORT). Each batch is written to a file and copied to the device with `cat`, in one write.
  evdev takes only whole 24-byte events, and bash flushes its output at every 0x0A byte, so a printf
  straight to the device would split any event holding a newline byte, such as y = 10 or 266.
- `hooks/apply.sh`: CoilShift's SDK 1.0 only expands a package with an apply hook or byte rules,
  so this service-only package needs one. CoilShift also runs it again when a setting changes. The
  service reads the password and picture size only at start, so the hook restarts it
  (`POST /v1/services/vnc/restart`) when they differ from what it started with.
- `hooks/remove.sh`: stops a leftover x11vnc from the package's copy and deletes the copy and the
  password file.

`test/run.sh` checks the exact touch bytes (including coordinates with a 0x0A byte), the x11vnc
arguments for each setting, that a taken port or a missing Tailscale address keeps x11vnc from
starting, and that the apply hook restarts the service only after a setting changed.
