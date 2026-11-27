# lock-chime-random

Play a **random `*.wav` from a `LockChime` folder on the USB** when the car locks, instead of the
single `LockChime.wav` the stock firmware supports.

**Status:** ✅ verified live on the car 2026-11-12 (standalone form running; 8 consecutive locks →
8 random picks). Build `2026.8.6-amd` (the owner's car).

> **This is NOT a libQtCarGUI byte patch** (unlike fart/boombox/horn). It's a tiny userspace daemon
> that swaps a file between locks. Read "How it works" before porting — the obvious gst path is a
> dead end.

> **2026.38.6 (checked live on the car, 2026-11-24): the USB lock sound is played by gst, not
> QtCarAudiod.** On each lock `ExternalSpeakerManager::lockStatusChanged` → `LockSoundPlayer` logs
> `attempting to play sound: lock`, then runs
> `gst-launch-1.0 filesrc location=/home/tesla/.Tesla/data/LockChime.wav ! … ! alsasink device="lock"`.
> PipeWire links its `lock` node to `TDM-playback`, the exterior speaker. The interior
> `FeedbackLockChime` is a separate stock chime, driven by `GUI_lockRequest`/`VAPI_lockIndication`.
> **gst runs as the UI's user, `tesla` (uid 1111)**, so `LockChime.wav` must be readable by
> `tesla`. Up to 1.1.1 the service copied clips as root under its supervisor's umask (077). The file
> came out `root:root 0700`, gst exited about 130 ms after starting, and the lock was silent. The
> remove hook's restore wrote over that same file, so the stock sound stayed unreadable too. 1.1.2
> puts every file in place through `lr_install`, with mode 0644 and the data folder's owner
> (`1111:1111`). `VAPI_lockIndication` reads `<invalid>` between events (it's only briefly 1 or 2),
> so polling it says nothing. Section "How it works" below describes 2026.8.6.

## Setup (in the car)

1. **Add a folder named `LockChime` to your USB stick** and put your clips in it. It can be on any
   partition the car mounts; the package looks through all of them. On the test car it's a separate
   partition labelled `LOCKCHIME`. To use another name, change the package's *USB folder name* setting.
2. **Clips must be WAV, mono, 48 kHz, 16-bit**, and short (under 5 seconds, like Tesla's own limit).
   Anything else is skipped. To convert one:
   ```sh
   ffmpeg -i in.mp3 -ac 1 -ar 48000 -c:a pcm_s16le -t 5 out.wav
   ```
3. **A USB lock sound must be on.** If it isn't yet, copy any `LockChime.wav` to the stick's top level
   and choose USB in **Toybox > Boombox > Lock Sound**. Without it the car never plays the file this
   package swaps.
4. **Install "Random lock chime" in CoilShift.** The line under its status says what's still missing
   (a USB lock sound, the folder, usable clips) or `Ready: N clip(s) in 'LockChime'`. It's re-checked
   when the service starts and after every lock.

Removing the package puts your own lock sound back.

---

## How it works (and the dead end to avoid)

The internal lock chime you hear is played **in-process by `QtCarAudiod` via ALSA**
(`Audio::playChime(LockChime)` → `ChimeChannel::doPlayChime` → `ChimeParameters::loadPcmData` →
`AlsaPcmData`). **It is not gstreamer**, and it is **not** the `libQtCarGUI`
`LockSoundPlayer::getCommand()` / `gst-launch … alsasink device="lock"` path — that is a *separate*
feature, the **external pedestrian-speaker (PES) lock sound**, gated by `GUI_pesLockSoundEnabled`.
Patching that gst path does nothing for the normal lock chime (verified: locking produced **no**
`gst-launch` process). **Don't go down the gst road.**

The key fact that makes a no-patch fix possible: **`QtCarAudiod` re-reads
`/home/tesla/.Tesla/data/LockChime.wav` from disk on *every* lock** — proven with `inotifywait`:
each lock is one `OPEN … ACCESS … CLOSE` on that file; it is not cached across locks. That file is
the writable local copy of the USB `LockChime.wav` (stock imports USB → `/home/.Tesla/data` on
plug). So:

> After each read (`CLOSE`), atomically stage a fresh random clip from the USB `LockChime/` folder
> into `/home/tesla/.Tesla/data/LockChime.wav`. The next lock reads the new file. The swap happens
> **between** locks → no race, no binary patch, reversible.

Trigger path (for reference, all in `libQtCarGUI` for the *gate*, playback in `QtCarAudiod`):
`ExternalSpeakerManager::lockStatusChanged()` fires on the lock DV and, **only if**
`VehicleUtils::customLockSoundEnabled()` (= `GUI_pesLockSoundEnabled || GUI_tronMode ||
GUI_HoHoHoMode`) and `VAPI_lockIndication == 1`, drives the chime. ⇒ **a custom lock sound must be
enabled in the UI for any of this to run.**

### Constraints discovered on this car (don't relearn them)
- `/home` is **`noexec`** → never `exec` the script; run it as `sh <script>` (the interpreter
  reads it as data). CoilShift's service runner already invokes hooks/services this way.
- `/home` is **`nosymfollow`** → the swap must copy file **content**; a symlink at the target is not
  followed.
- USB is **`ro` exfat** at `/mnt/media/usb-sdb3` (label `LOCKCHIME`) → read the folder, never write it.
- busybox `ps` (no args) does **not** list these processes — check via `/proc/<pid>/cmdline` or `kill -0`.
- `pkill -f cs-lockrand` matches **your own shell** (its cmdline contains the path) and kills your
  session — stop the daemon **by PID** (scan `/proc`, skip `$$`/`$PPID`), as `lock-chime-random.sh` does.
- Backgrounding (`&`) inside an SSH command truncates that command's stdout — start detached, verify
  in a separate call.
- Only **mono / 48 kHz / 16-bit** clips are staged (what audiod plays here); a stereo file (e.g.
  `PH.wav`) is auto-skipped. Convert clips to mono/48k/16 to include them.

---

## Two forms

### 1. `lock-chime-random.sh` — standalone (unsigned), run on the car
Installs `cs-lockrand.sh` to `/home/tesla/.cs-lockrand/`, backs up the stock sound, and
(re)starts the daemon via `setsid sh`. Idempotent + readback-verified. **Survives a UI restart and
sleep, but NOT a reboot** — re-run after a reboot/OTA (this is what [`../apply-all.sh`](../apply-all.sh)
is for). This is the form running on the car today.

```sh
# on the car, as root (after staging the repo - see ../apply-all.sh header):
sh lock-chime-random.sh apply
# stop / revert:
#   kill the daemon by pid (NOT pkill -f from a combined command), then:
#   cp /home/tesla/.cs-lockrand/LockChime.orig.wav /home/tesla/.Tesla/data/LockChime.wav
```
Runtime files on the car: `/home/tesla/.cs-lockrand/{cs-lockrand.sh, LockChime.orig.wav,
cs-lockrand.log, daemon.out}`.

### 2. `coilshift/io.github.ivan-prodanov.lock-chime-random/` — the durable form
A CoilShift **service** package — this is the right home for it, because:

- **CoilShift CAN run daemons** → `hooks.service: [{ "id": "lockrand", "run": "bin/cs-lockrand.sh",
  "restart": "always" }]`. ssp supervises it like `dev.coilshift.ssh-key`'s `keys.sh` service
  (a long-running `while`/watch loop). `restart: "always"` keeps it up.
- **CoilShift re-applies at startup** → ssp re-establishes enabled mods and their services on boot
  (`ssp-watchdog` + one `ssp-client` per mod), so **this form persists across reboot** — the thing
  the standalone form can't do. (That's the whole reason to prefer the package.)

Package contents:
```
io.github.ivan-prodanov.lock-chime-random/
  coilshift.json          schema 1; capabilities ["service"]; hooks.apply + hooks.service + hooks.remove; one "folder" setting
  bin/cs-lockrand.sh      the service (restart:always): prime + inotify-close restage loop; TERM/INT/HUP -> restore stock from CSDK_STATE
  hooks/apply.sh          back up stock LockChime.wav to CSDK_STATE (once); set the setup status
  hooks/remove.sh         restore stock LockChime.wav from CSDK_STATE
  lib/lockrand.sh         shared: folder setting, USB search, clip format check, setup status (lr_status)
  lib/csdk.sh             vendored SDK 1.0 helper, byte for byte what `csdk init` writes
```

**Status:** passes `csdk lint --strict` and `csdk test` (`apply,apply`, `apply,remove`); built by
`tools/build.sh` into `dist/public/`. No signing: it reaches the car from a repository you
publish (see the top-level README).

**Still to confirm on the car** (the SDK docs don't settle these):
1. **Stop delivers `TERM`.** The SDK docs give the service environment (`CSDK_PACKAGE_DIR`,
   `CSDK_STATE`, `CSDK_SETTINGS`, `CSDK_API_SOCKET`, `CSDK_TOKEN`, ...) and say removal stops the
   services first, but not which signal. The teardown that restores the stock sound runs on
   `TERM`/`INT`/`HUP`; `hooks/remove.sh` restores it again either way.
2. **The package tree is `noexec`** (per the car API docs). The service is a bash script, like the
   SDK's own service examples, so the daemon runs it through bash.
3. **Settings UI.** The `folder` setting (default `LockChime`) shows up in CoilShift's settings
   with no extra work.

Nothing here writes the read-only rootfs; removal restores the stock sound and leaves the backup.

---

## Prerequisites (both forms)
- A **custom lock sound enabled** in the car UI (lock sound = USB) — otherwise `QtCarAudiod` never
  reads `LockChime.wav` and there's nothing to randomize.
- A folder named **`LockChime`** on the USB stick containing `*.wav` clips, **mono / 48 kHz /
  16-bit** (others are skipped). On the test car: `/mnt/media/usb-sdb3/LockChime/` (the stick is
  also labeled `LOCKCHIME`).

## Verify on the car
```sh
# daemon + its watcher alive (busybox ps won't show them; use /proc):
for d in /proc/[0-9]*; do tr '\0' ' ' < "$d/cmdline" 2>/dev/null | grep -q 'inotifywait .*Tesla/data' && echo up; done
# each lock appends a pick:
cat /home/tesla/.cs-lockrand/cs-lockrand.log        # standalone
# or, for the package, the per-package csdk log stream.
# prove randomization without locking (a read == a lock):
for i in 1 2 3 4 5; do m=$(md5sum /home/tesla/.Tesla/data/LockChime.wav|cut -d' ' -f1); \
  md5sum /mnt/media/usb-sdb3/LockChime/*.wav|awk -v m="$m" '$1==m{print $2}'; sleep 1; done
```
