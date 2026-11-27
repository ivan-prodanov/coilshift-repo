# direct-summon

**Direct Summon** (`io.github.ivan-prodanov.direct-summon`): summonhelper as a CoilShift
package, on top of Direct Link. Built and tested together with Direct Link: everything (design,
plan, runbook, build, tests) is in [`../direct-link/`](../direct-link/README.md).

- Requires Direct Link at the same gateway build; its apply refuses a mismatched pair.
- While installed, stock `webrtc-comms` is stopped at every boot and whenever it takes
  `com.tesla.Webrtc` back ("take it back"), so Tesla-app Summon and the Tesla app's live camera
  are off. Removing the package hands the name back.
- Motion is unchanged from today's install: the same summonhelper binary and the same run and
  finish scripts (D2 deadman, D3, H1, H4, the takeover; the finish sends cmd_abort and the
  disconnect after any exit).
