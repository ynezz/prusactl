---
name: prusactl
description: Operate a Prusa 3D printer (CORE One, MK4S, XL, MINI and others) through the prusactl MCP tools or CLI. Use when asked to check print status or progress, look at the printer's camera, start, pause, resume, or stop a print, watch a print for failures such as spaghetti or detached parts, upload or delete printer files, heat or move the printer, or check and update the printer's firmware.
---

# Operating a Prusa printer with prusactl

You are expected to act, not to gate every step on a question. The tools are
`mcp__prusa__*` when the prusactl MCP server is connected (names below are the
tool names); otherwise the CLI runs the same tools (`prusactl status`,
`prusactl camera`, `prusactl firmware`, `prusactl help`). Every printer argument
is optional with one printer. Results say which route answered (`via`: direct
on the local network, or Prusa Connect).

If a tool says something is not set up, tell the user the command it names
(`prusactl setup`, `prusactl login`). Never try to set up credentials yourself.

Prusa Connect sign-in can be done by an agent without handling a password, but
only when the user asks you to sign in. `prusactl login --manual` prints a Prusa
Account link as the first stdout line and then reads one line from stdin:

1. Run it in the background with stdin kept open, for example a FIFO
   (`mkfifo f; prusactl login --manual < f`, with another process holding `f`
   open for writing).
2. Read the first stdout line, the link. Open it in a browser tab you can
   inspect (a Chrome DevTools Protocol tab works) and let the user approve.
3. Read the tab's final address
   (`https://connect.prusa3d.com/login/auth-callback?code=...&state=...`) and
   write it plus a newline to the FIFO. The command signs in and exits.

## Check status

1. `get_printer`: read `summary` (state, temperatures, job progress). States:
   IDLE, READY, PRINTING, PAUSED, FINISHED, STOPPED, ATTENTION (a question is on
   the printer's screen), BUSY, OFFLINE.
2. ATTENTION means a dialog. Through Connect, `get_printer` shows it in
   `dialog_info` and `respond_to_dialog` presses a button. Read the text first.
3. For history or timing use `list_jobs`, `get_telemetry`, `list_events`
   (Connect only).

## Start a print

1. `get_printer`: the state must be idle (IDLE/READY, or FINISHED/STOPPED).
2. `get_camera_snapshot` and **look at the image**: is the plate empty and clean,
   nothing in the way, the right sheet on? If the image is old or dark, say so.
3. If the file is on this computer: `upload_file` with `then: "print"`. If it is
   already on the printer: `list_printer_files`, then `start_print`.
4. After FINISHED or STOPPED the tool refuses until `plate_clear: true`. Pass it
   only after step 2 showed an empty plate, or the user said so in this
   conversation. It is your assertion; nothing checks it.
5. Confirm with `get_printer` that the state is PRINTING.

## Monitor a print

Loop while the state is PRINTING (every 2-5 minutes; faster on the first layers,
where most failures start):

1. `get_printer`: note progress, temperatures, state. Stop looping on any state
   other than PRINTING or PAUSED.
2. Every few polls, and always on the first layers, `get_camera_snapshot` and
   look at it: the part stuck to the bed, no spaghetti or loose strands, nozzle
   not carrying a blob, nothing knocked over.
3. Temperatures far from their targets, or progress that stops moving for many
   minutes, are warnings too.
4. On a suspected failure: **pause first** (`control_print` action `pause`;
   reversible), then take another snapshot to confirm, then tell the user what
   you saw and ask. Resume if it was a false alarm.
5. Use `control_print` action `stop` only for a clear failure (spaghetti, part
   detached, crash) or when the user tells you to. Stopping is final.
6. When it finishes, say so, with the last snapshot if useful. Do not start
   anything else until the plate is cleared.

## Firmware update

1. `get_firmware_status`: running and latest version, `state`, `update_available`.
2. `update_firmware` (Prusa Connect only; `version` picks another release) has
   Connect copy the `.bbf` to the printer's USB drive and then installs it with
   `FLASH`, which restarts the printer. It waits for the copy and does nothing
   when the printer is up to date.
3. `FLASH` is accepted only while the printer is idle, ready, finished or
   stopped. During a print the file is left on the drive (`installed: false`):
   tell the user, and run `update_firmware` again once the print has ended.
   Never restart the printer mid-print. The printer may ask for a confirmation
   on its screen.

## Gotchas

- **`plate_clear` is your own claim.** The printer cannot check it. Verify with a
  fresh snapshot you have read, or the user's word. `SET_PRINTER_READY` and
  `add_to_queue set_ready` mean the same thing.
- **`run_gcode` is a print job, not a console.** It writes
  `/usb/prusactl-macro.gcode` (overwriting it) and starts it, so it needs an idle
  printer, appears in history, and runs whatever G-code you give it. Moves hit
  real hardware: check the camera first.
- **`api_request` is a raw escape hatch.** No safety checks, and a DELETE or PUT
  really happens. Prefer the dedicated tools; use it with GET to look.
- **Deletes are permanent.** `delete_printer_files`, `delete_connect_files`, and
  `remove_from_queue` have no trash. List first; delete only what the user named.
- **Stop is final, pause is not.** Pause when unsure.
- **The camera needs a source.** `get_camera_snapshot` uses an RTSP camera saved
  with `prusactl setup --camera rtsp://<camera-ip>/live` (needs ffmpeg), else Prusa
  Connect. Without either it errors; tell the user how to fix it, and do not
  start prints blind unless they say to.
- **Some things are Connect-only** (dialogs, queue, history, telemetry); others
  direct-only (`run_gcode`, `download_printer_file`). Errors name the route.
- **Never print secrets.** Tool output is redacted; keep it that way.
