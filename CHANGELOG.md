# Changelog

## Unreleased

- **The camera works without Prusa Connect.** The Buddy3D camera is a separate
  Wi-Fi device that talks to Connect itself and serves RTSP at
  `rtsp://<camera-ip>/live`; Buddy firmware's PrusaLink has no camera endpoint,
  which is why `get_camera_snapshot` and `prusactl camera` used to need
  Connect. `prusactl setup --camera rtsp://...` (or `PRUSACTL_CAMERA_URL`) saves
  the address and prusactl grabs a frame with ffmpeg. A PrusaLink that serves
  `/api/v1/cameras/snap` is used first, and Connect is still the fallback.
  `prusactl status` shows the saved camera. Only plain `rtsp://` addresses are
  accepted (ffmpeg doesn't verify the certificate of `rtsps://`), and one with a
  user or password is refused: ffmpeg would get it on its command line, readable
  by any local user, and it would be stored in plain text. The Buddy3D takes none.
- `prusactl login --manual` signs in without a password passing through the
  terminal, so an AI agent can run it. It prints the Prusa Account link on its
  own line, then reads the address the browser ends on
  (`https://connect.prusa3d.com/login/auth-callback?code=...`) from stdin, so an
  agent can keep stdin open, read the address from a browser tab, and write it
  back. `--no-open` stops it starting a browser. A loopback mode doesn't exist
  because Prusa Account rejects a localhost redirect for the Connect client
  ("Mismatching redirect URI").
- `prusactl login` without `--manual` says so when it has no terminal to ask for
  a password on, rather than failing on the prompt.
- `prusactl status` and the printer summary show how long a running print has
  left, how long it has run, and when it should finish on the local clock, for
  example `PRINTING 89%, 23m left (ends ~12:41), 2h10m elapsed, nozzle 250/250°C,
  bed 85/85°C`. `get_printer` and `list_printers` gain `time_elapsed` and
  `ends_at` next to `time_remaining`. Missing or negative values are left out.
- **Camera snapshots work again.** Around 2026-09-30 Prusa moved cameras to a
  new camera service, and the endpoints `get_camera_snapshot` and
  `prusactl camera` used stopped serving images (404), which prusactl reported
  as a possible API change. Snapshots now come from the camera service, as on
  the Connect website: Connect's GraphQL API gives each camera's latest
  snapshot URL, fetched with your session. The old endpoints remain the
  fallback. The session is only ever sent to Prusa's own hosts.

## 0.2.1 (2026-09-29)

- **The question about the plate came back.** 0.2.0 reworded tool errors on
  their way out, and the terminal decided whether to ask "is the plate clear?"
  by reading that same wording — so after a finished or stopped print,
  `prusactl print` refused instead of asking. The rewording now happens once,
  where the message is shown, and a test holds the two apart.
- `prusactl cloud --printer` works. It was advertised as the way to pick a team
  with several printers and did nothing on `cloud ls`, while `cloud rm` refused
  the flag outright.
- `prusactl download` runs the same code as `prusactl files get` again. They are
  one command under two names, and 0.2.0 left them as two implementations:
  `download` lost `--json`, and `files get X out/` quietly wrote a file named
  `out` instead of writing into the folder.
- `get_transfers` says when Prusa Connect's reply has changed shape instead of
  reporting that nothing is being transferred, which is indistinguishable from
  the truth and would tell an agent a file had arrived. It also keeps the
  transfer's id, and the printer's own time remaining.
- A firmware command the printer rejected is reported as REJECTED. `cmd send`
  read the state the command was filed under, not the event that ended the
  wait, so a rejected command came back as CREATED.
- `start_print` through Prusa Connect waits for the printer like the direct
  route does. Connect's record lags, so a print that started fine was reported
  as "isn't printing yet".
- `control_print` refuses on both routes when there is no job to act on, and no
  longer returns a PrusaLink job number under the same name as a Prusa Connect
  history id: they belong to different collections and only one works with
  `jobs`.
- Flags that don't apply to a subcommand are refused rather than ignored, as
  arguments already were: `cmd ls --async`, `cloud rm --limit`, `files ls
  --overwrite`, and `cmd send --async --timeout` (the timeout is the wait).
- `prusactl camera --json` into a pipe says what it can't do instead of writing
  a JPEG where JSON was asked for, and `prusactl dialog` reports the button the
  printer matched rather than what was typed.

## 0.2.0 (2026-09-29)

- The CLI does everything the agent can. `printers`, `print`, `start`, `pause`,
  `resume`, `stop`, `gcode`, `dialog`, `files`, `cloud`, `queue`, `jobs`,
  `events`, `telemetry`, `transfers`, `camera` and `cmd` run the very tools the
  MCP server exposes, over an in-memory connection, so both surfaces share one
  implementation and one set of safety checks. `--json` gives the tool's own
  result; without it each command prints a table.
- `prusactl status --json` adds everything the printer reports, including the
  chamber temperature and the running job.
- `prusactl download` is now `prusactl files get`, alongside `files ls`, `put`
  and `rm`. The old name still works.
- `delete_connect_files` deletes files from Prusa Connect's cloud storage, which
  had no way to delete what `upload_file` left there.
- `get_printer` reports the same handful of facts, in the same place, whichever
  route answered: `summary` carries state, temperatures, and the running job.
  The route's own reply is still there in full.
- `get_command` works. It asked for an endpoint Prusa Connect doesn't have;
  Connect records a command's life in the event log, so that is where it looks.
- Mistyped flags are refused by every command. `status --jsom` used to be
  ignored, printing the wrong kind of output without a word.
- Errors a person reads no longer name MCP tools they can't run, and a file the
  printer holds open explains itself instead of only saying "File is busy".
- `prusactl printers` lists the printer reached directly, which it used to skip,
  and says which route each one came from.
- The tests run on Windows in CI. Four of them used to write into the real
  prusactl directory there, overwriting the config and credentials of whoever
  ran them; nothing had run them on Windows in 36 commits.
- Tools that answered in one shape directly and another through Prusa Connect
  now answer the same way either way, so a caller reads them once.
  `get_transfers` reported nothing at all over Connect, where it matters most,
  because Connect's list is the transfer history and every entry in it has
  finished; it also carried the whole sliced file's metadata, down to the
  outline of every object on the plate. `upload_file` over Connect now says
  where the file landed and gives back the hash `cloud rm` and `queue add
  --hash` take. `control_print` and `start_print` report what they acted on and
  how it went over Connect too, so `prusactl print` no longer says "Printing"
  when the printer stopped on a question.
- `prusactl files get` honours the flags it advertises. It re-parsed its
  arguments against the old `download` command, so `--overwrite` was dropped
  and `--printer` and `--via` were ignored.
- `prusactl cloud` can say whose storage it means, with `--printer` or `--team`.
  With more than one printer it used to ask for an argument the CLI didn't have.
- `prusactl cmd send --async` returns the command id that `cmd status` follows;
  there was no way to get one before.
- Commands refuse arguments they don't read and values that aren't numbers.
  `prusactl telemetry 60` ignored the 60, and `--limit 5O` quietly returned the
  first page.
- Errors reaching the terminal name flags, not the tools' JSON arguments.

## 0.1.6 (2026-09-28)

- `connection_status` no longer hands the agent the whole Prusa Account record
  (email address, account id, terms dates, team organization ids). It returns
  who is signed in and the default team, as its description always promised.
- The MCP bundle works on ARM Linux, such as a Raspberry Pi; it previously
  installed and then failed to start there.
- Every list pages the same way: one cap (500), shared field descriptions, and
  `next_offset` on Connect-backed lists too. `list_jobs` with a huge limit now
  returns a page instead of "result too large".
- `get_queue`, `add_to_queue` and `remove_from_queue` answer in one shape, and
  `get_queue` no longer reports a route it can't vary.
- The README discloses that `run_gcode` leaves `/usb/prusactl-macro.gcode` on
  the printer, lists `prusactl version`, and qualifies "no browser involved".

## 0.1.5 (2026-09-28)

- The MCP bundle no longer tells the agent Prusa Connect is unavailable there.
  It reads the same saved session, so the camera, queue and history work
  whenever an installed prusactl has signed in; only the sign-in needs a
  terminal.
- `control_print` takes pause, resume, or stop. "continue", an undocumented
  second name for resume, is gone, and a bad action says so before the printer
  is asked anything.
- `get_queue` takes `limit` and `offset` like the other lists.
- `api_request` reports which route it used, and the docs no longer claim every
  tool does.
- `get_printer` describes how the two routes name their fields differently, so
  a result from one route isn't read as if it came from the other.
- `upload_file` and `list_connect_files` say that uploading through Connect
  leaves a copy in the team's storage, which only the Connect web app deletes.
- Smaller: identical folder paths from both routes, `download_printer_file`
  explains it needs the direct connection, and the sign-in prompt names the
  host the password is actually sent to.

## 0.1.4 (2026-09-28)

- Homebrew installs from source, so `brew` no longer prints a deprecation
  warning on every command. Coming from 0.1.3 or earlier, switch once with
  `brew uninstall --cask prusactl && brew update && brew install trevin-lee/tap/prusactl`.
- Running `setup` again removes the password it replaces, so a changed printer
  address no longer leaves credentials behind, and `--forget` clears everything.
- `via` is honoured everywhere: a tool that only works one way now refuses the
  other route instead of ignoring it, and a bad value is always an error.
- `list_printer_files` pages the same way over Prusa Connect as directly.
- `prusactl status` shows the printer's state through Connect when the direct
  route isn't set up.
- `prusactl api` parses flags like every other command (`--foo` was sent to the
  printer as the HTTP method), and checks the HTTP method.
- The MCP bundle's messages point at its own settings instead of terminal
  commands that don't apply there.
- `prusactl logout` no longer claims to remove a session that wasn't saved, and
  the version prints the same way however prusactl was built.
- The README and help describe what the CLI actually does, mark the examples
  that need Prusa Connect, and cover uninstalling and the one-printer limit.

## 0.1.3 (2026-09-28)

- When Prusa changes an API, prusactl says so plainly instead of misbehaving:
  "Prusa Connect answered in a way prusactl doesn't recognize: …", naming the
  request and what was unexpected, with what to do. It covers removed Connect
  endpoints, changed response formats and missing fields (a renamed printer
  list no longer reads as "no printers"), Prusa Account sign-in and token
  changes, and firmware changes to the printer's local API.
- A rejected sign-in app ID no longer deletes the saved session.

## 0.1.2 (2026-09-28)

- The plate check applies to every tool that starts a job or moves the printer:
  after a finished or stopped print, `send_command` HOME, MOVE, MOVE_Z,
  MESH_BED_LEVELING, and START_PRINT need `plate_clear`, like `start_print`.
  Marking the printer ready is documented as the same confirmation, and
  `api_request` as raw access that skips the checks.
- The README explains how to uninstall completely, including the saved
  password and session.

## 0.1.1 (2026-09-28)

- Windows: `setup` and `login` work over SSH and in services, where Windows
  has no Credential Manager; credentials go in the private `secrets.json`.
  Tested on Windows 11 ARM64: the test suite, setup, status, and the MCP server.
- `list_printer_files` pages the printer's folders (default 50, `next_offset`,
  at most 500), and every tool result is capped at 256 KB with a request to
  narrow it.
- `setup --forget` says "API key" when it removes one.

## 0.1.0 (2026-09-28)

First release.

- Talk to the printer directly over PrusaLink: status, files, upload and print,
  pause/resume/stop, and G-code while the printer is idle. Works over a VPN
  such as Tailscale.
- Download files from the printer's storage (`prusactl download`,
  `download_printer_file`), for example to read the slicer settings embedded
  in a finished print.
- Optional Prusa Connect support with terminal sign-in: camera, on-screen
  dialogs, print queue, history, events, telemetry, and firmware commands.
- MCP server (`prusactl mcp`) whose tools pick the direct route when the
  printer answers and fall back to Connect.
- Credentials stay out of transcripts: API keys, tokens and passwords are
  masked in every tool result, error, and `prusactl api` response (`--raw`
  shows them).
- Runs on headless Linux such as a Raspberry Pi: with no system keychain,
  credentials go in a private `secrets.json` (`PRUSACTL_KEYRING=file` forces
  it).
- Distributed as binaries, a Homebrew cask, an MCP bundle, and an MCP Registry
  listing.
